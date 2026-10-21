// Package broker gives workspace pods short-lived presigned URLs for their own
// workspace's objects, so S3 credentials never enter a pod.
//
// Each call carries the pod's broker token plus the workspace and epoch it
// claims. The broker reads the Workspace live from the API server, checks the
// token for that workspace and epoch, and refuses epochs other than the
// current one, so a pod left over from an earlier wake can't read or write.
package broker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Metadata keys a pod sends with every call.
const (
	MDWorkspace = "x-sbx-workspace" // "namespace/name"
	MDEpoch     = "x-sbx-epoch"
)

// Workspaces reads a Workspace, uncached, by namespace and name.
type Workspaces interface {
	Get(ctx context.Context, namespace, name string) (*sbxv1.Workspace, error)
}

// Config configures the broker.
type Config struct {
	Keyring        token.Keyring
	Store          objstore.Store
	Workspaces     Workspaces
	PresignTTL     time.Duration
	MaxBundleBytes int64
	MaxDepsBytes   int64
}

// Server implements BrokerService.
type Server struct {
	brokerv1.UnimplementedBrokerServiceServer
	cfg Config
}

// New returns a broker.
func New(cfg Config) *Server {
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 15 * time.Minute
	}
	return &Server{cfg: cfg}
}

// Register adds BrokerService to g. The server must be built with
// UnaryInterceptor so every call is authenticated.
func (s *Server) Register(g *grpc.Server) { brokerv1.RegisterBrokerServiceServer(g, s) }

type callerKey struct{}

// caller is an authenticated pod.
type caller struct {
	ws    *sbxv1.Workspace
	epoch int64
}

// UnaryInterceptor authenticates every BrokerService call.
func (s *Server) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if !isBrokerMethod(info.FullMethod) {
			return next(ctx, req)
		}
		c, err := s.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, callerKey{}, c), req)
	}
}

func isBrokerMethod(m string) bool {
	return strings.HasPrefix(m, "/"+brokerv1.BrokerService_ServiceDesc.ServiceName+"/")
}

func (s *Server) authenticate(ctx context.Context) (*caller, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	first := func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	tok, ok := token.FromHeader(first("authorization"))
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	ns, name, ok := splitWorkspace(first(MDWorkspace))
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing or malformed "+MDWorkspace)
	}
	epoch, err := strconv.ParseInt(first(MDEpoch), 10, 64)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing or malformed "+MDEpoch)
	}
	ws, err := s.cfg.Workspaces.Get(ctx, ns, name)
	if err != nil {
		// Don't reveal whether the workspace exists.
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	if err := s.cfg.Keyring.Verify(tok, token.Broker, string(ws.UID), epoch); err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	if epoch != ws.Status.Epoch {
		return nil, status.Errorf(codes.PermissionDenied, "epoch %d is not the workspace's current epoch", epoch)
	}
	return &caller{ws: ws, epoch: epoch}, nil
}

func splitWorkspace(v string) (ns, name string, ok bool) {
	ns, name, ok = strings.Cut(v, "/")
	return ns, name, ok && ns != "" && name != ""
}

func from(ctx context.Context) *caller {
	c, _ := ctx.Value(callerKey{}).(*caller)
	return c
}

func (s *Server) presigned(r objstore.Request, key string) *brokerv1.PresignedRequest {
	h := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 && k != "Host" {
			h[k] = vs[0]
		}
	}
	return &brokerv1.PresignedRequest{Method: r.Method, Url: r.URL, Headers: h, ExpiresAt: timestamppb.New(r.Expires), Key: key}
}

// exists reports whether key holds an object.
func (s *Server) exists(ctx context.Context, key string) (bool, error) {
	_, err := s.cfg.Store.Head(ctx, key)
	if errors.Is(err, objstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, status.Error(codes.Unavailable, "object store: "+err.Error())
	}
	return true, nil
}

// GetRestorePlan says where this epoch's files come from: the base commit
// (a cached bundle, or origin) and the latest snapshot, if any.
func (s *Server) GetRestorePlan(ctx context.Context, _ *brokerv1.GetRestorePlanRequest) (*brokerv1.GetRestorePlanResponse, error) {
	ws := from(ctx).ws
	base := ws.Status.Base
	if base == nil || base.SHA == "" {
		return nil, status.Error(codes.FailedPrecondition, "workspace has no base commit yet")
	}
	plan := &brokerv1.GetRestorePlanResponse{
		OriginUrl: base.URL,
		BaseSha:   base.SHA,
		Branch:    base.Branch,
		Depth:     ws.Spec.Source.Depth,
	}

	baseKey := snapstore.BaseBundleKey(base.URL, base.SHA)
	cached, err := s.exists(ctx, baseKey)
	if err != nil {
		return nil, err
	}
	switch {
	case cached:
		r, err := s.cfg.Store.PresignGet(ctx, baseKey, s.cfg.PresignTTL)
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		plan.BaseBundle = s.presigned(r, baseKey)
	case !base.Shallow:
		plan.UploadBase = true
	}

	if snap := ws.Status.LastSnapshot; snap != nil && snap.BundleKey != "" {
		r, err := s.cfg.Store.PresignGet(ctx, snap.BundleKey, s.cfg.PresignTTL)
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		plan.SnapshotBundle = s.presigned(r, snap.BundleKey)
		plan.SnapshotSha256 = snap.SHA256
	}
	return plan, nil
}

// PresignSnapshotUpload returns a create-only, checksum-checked URL for a
// snapshot bundle under the caller's own workspace prefix.
func (s *Server) PresignSnapshotUpload(ctx context.Context, r *brokerv1.PresignSnapshotUploadRequest) (*brokerv1.PresignSnapshotUploadResponse, error) {
	c := from(ctx)
	if !isSHA256(r.GetSha256()) || r.GetSizeBytes() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "sha256 and size_bytes are required")
	}
	if max := s.cfg.MaxBundleBytes; max > 0 && r.GetSizeBytes() > max {
		return nil, status.Errorf(codes.ResourceExhausted, "bundle is %d bytes, over the %d-byte limit", r.GetSizeBytes(), max)
	}
	key := snapstore.BundleKey(string(c.ws.UID), r.GetSha256())
	return s.presignCreate(ctx, key, r.GetSha256())
}

// PresignBaseUpload returns a create-only URL for the workspace's base bundle.
func (s *Server) PresignBaseUpload(ctx context.Context, r *brokerv1.PresignBaseUploadRequest) (*brokerv1.PresignBaseUploadResponse, error) {
	base := from(ctx).ws.Status.Base
	if base == nil || base.SHA == "" {
		return nil, status.Error(codes.FailedPrecondition, "workspace has no base commit yet")
	}
	if !isSHA256(r.GetSha256()) || r.GetSizeBytes() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "sha256 and size_bytes are required")
	}
	resp, err := s.presignCreate(ctx, snapstore.BaseBundleKey(base.URL, base.SHA), r.GetSha256())
	if err != nil {
		return nil, err
	}
	return &brokerv1.PresignBaseUploadResponse{Upload: resp.Upload, AlreadyExists: resp.AlreadyExists}, nil
}

// LookupDeps looks dependency archives up in the shared cache and returns a
// download URL for each hit. The cache is shared by every workspace (keys
// say what produced an archive, not who), which is why it is write-once.
func (s *Server) LookupDeps(ctx context.Context, r *brokerv1.LookupDepsRequest) (*brokerv1.LookupDepsResponse, error) {
	if len(r.GetKeys()) > 8 {
		return nil, status.Error(codes.InvalidArgument, "at most 8 keys per lookup")
	}
	resp := &brokerv1.LookupDepsResponse{}
	for _, k := range r.GetKeys() {
		obj, err := depsObject(k)
		if err != nil {
			return nil, err
		}
		entry := &brokerv1.DepsEntry{Key: k}
		o, err := s.cfg.Store.Head(ctx, obj)
		switch {
		case errors.Is(err, objstore.ErrNotFound):
		case err != nil:
			return nil, status.Error(codes.Unavailable, "object store: "+err.Error())
		default:
			req, err := s.cfg.Store.PresignGet(ctx, obj, s.cfg.PresignTTL)
			if err != nil {
				return nil, status.Error(codes.Unavailable, err.Error())
			}
			entry.Hit, entry.Download, entry.Sha256 = true, s.presigned(req, obj), o.SHA256
		}
		resp.Entries = append(resp.Entries, entry)
	}
	return resp, nil
}

// PresignDepsUpload returns a create-only, checksum-checked URL for a
// dependency archive, or AlreadyExists if another pod got there first.
func (s *Server) PresignDepsUpload(ctx context.Context, r *brokerv1.PresignDepsUploadRequest) (*brokerv1.PresignDepsUploadResponse, error) {
	obj, err := depsObject(r.GetKey())
	if err != nil {
		return nil, err
	}
	if !isSHA256(r.GetSha256()) || r.GetSizeBytes() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "sha256 and size_bytes are required")
	}
	if max := s.cfg.MaxDepsBytes; max > 0 && r.GetSizeBytes() > max {
		return nil, status.Errorf(codes.ResourceExhausted, "archive is %d bytes, over the %d-byte limit", r.GetSizeBytes(), max)
	}
	resp, err := s.presignCreate(ctx, obj, r.GetSha256())
	if err != nil {
		return nil, err
	}
	return &brokerv1.PresignDepsUploadResponse{Upload: resp.Upload, AlreadyExists: resp.AlreadyExists}, nil
}

func depsObject(k *brokerv1.DepsKey) (string, error) {
	if _, ok := depcache.Lookup(k.GetProfile()); !ok || !depcache.ValidKey(k.GetKey()) {
		return "", status.Errorf(codes.InvalidArgument, "invalid dependency key %q/%q", k.GetProfile(), k.GetKey())
	}
	return snapstore.DepsKey(k.GetProfile(), k.GetKey()), nil
}

// presignCreate returns a create-only, checksum-checked upload URL for key,
// or AlreadyExists (with only the key set) if the content-addressed object is
// already stored.
func (s *Server) presignCreate(ctx context.Context, key, sha256 string) (*brokerv1.PresignSnapshotUploadResponse, error) {
	found, err := s.exists(ctx, key)
	if err != nil {
		return nil, err
	}
	if found {
		return &brokerv1.PresignSnapshotUploadResponse{AlreadyExists: true, Upload: &brokerv1.PresignedRequest{Key: key}}, nil
	}
	req, err := s.cfg.Store.PresignPut(ctx, key, s.cfg.PresignTTL, objstore.Cond{IfNoneMatch: true, SHA256: sha256})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &brokerv1.PresignSnapshotUploadResponse{Upload: s.presigned(req, key)}, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// KubeWorkspaces reads Workspaces straight from the API server (no cache), so
// an epoch change is visible to the broker immediately.
type KubeWorkspaces struct{ Reader client.Reader }

// Get implements Workspaces.
func (k KubeWorkspaces) Get(ctx context.Context, namespace, name string) (*sbxv1.Workspace, error) {
	ws := &sbxv1.Workspace{}
	if err := k.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, ws); err != nil {
		return nil, err
	}
	return ws, nil
}
