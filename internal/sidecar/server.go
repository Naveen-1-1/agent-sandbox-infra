package sidecar

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Server is the sidecar's gRPC server.
type Server struct {
	sidecarv1.UnimplementedSidecarServiceServer
	cfg    Config
	broker brokerv1.BrokerServiceClient
	snaps  *Snapshotter
	grpc   *grpc.Server
}

// NewServer builds the sidecar server. Every SidecarService call must
// present cfg.SidecarToken; gRPC health is open for kubelet probes.
func NewServer(cfg Config, broker brokerv1.BrokerServiceClient) *Server {
	cfg.defaults()
	s := &Server{cfg: cfg, broker: broker, snaps: NewSnapshotter(cfg, broker)}
	s.grpc = grpc.NewServer(grpc.ChainUnaryInterceptor(s.auth), grpc.ChainStreamInterceptor(s.authStream))
	sidecarv1.RegisterSidecarServiceServer(s.grpc, s)
	healthv1.RegisterHealthServer(s.grpc, health.NewServer())
	return s
}

func (s *Server) check(ctx context.Context, method string) error {
	if strings.HasPrefix(method, "/grpc.health.v1.Health/") {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) > 0 {
		if tok, ok := token.FromHeader(v[0]); ok && token.Equal(tok, s.cfg.SidecarToken) == nil {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid sidecar token")
}

func (s *Server) auth(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if err := s.check(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return next(ctx, req)
}

func (s *Server) authStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	if err := s.check(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	return next(srv, ss)
}

// Serve runs until ctx ends. It also uploads the base bundle in the
// background if the restore asked for one.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	defer s.snaps.Close()
	bg, stopBG := context.WithCancel(ctx)
	defer stopBG()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.uploadBase(bg)
	}()

	errc := make(chan error, 1)
	go func() { errc <- s.grpc.Serve(l) }()
	var err error
	select {
	case <-ctx.Done():
		s.grpc.GracefulStop()
		err = <-errc
	case err = <-errc:
	}
	stopBG()
	<-done
	if errors.Is(err, grpc.ErrServerStopped) {
		err = nil
	}
	return err
}

// uploadBase caches the workspace's base commit as a bundle so later wakes
// don't depend on the upstream remote. Best effort: failures are logged.
func (s *Server) uploadBase(ctx context.Context) {
	vol, ok, err := ReadVolume(s.cfg.Root)
	if err != nil || !ok || !vol.PendingBaseUpload {
		return
	}
	work := filepath.Join(s.cfg.Scratch, "base-upload")
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		s.cfg.Log.Warn("base upload", "err", err)
		return
	}
	path, size, sum, err := snapshot.CreateBaseBundle(ctx, s.cfg.Root, vol.BaseSHA, work)
	if err != nil {
		s.cfg.Log.Warn("creating base bundle", "err", err)
		return
	}
	resp, err := s.broker.PresignBaseUpload(ctx, &brokerv1.PresignBaseUploadRequest{Sha256: sum, SizeBytes: size})
	if err != nil {
		s.cfg.Log.Warn("presigning base upload", "err", err)
		return
	}
	if !resp.GetAlreadyExists() {
		u := resp.GetUpload()
		if err := upload(ctx, s.cfg.HTTP, u.GetMethod(), u.GetUrl(), u.GetHeaders(), path); err != nil {
			s.cfg.Log.Warn("uploading base bundle", "err", err)
			return
		}
	}
	vol.PendingBaseUpload = false
	if err := WriteVolume(s.cfg.Root, vol); err != nil {
		s.cfg.Log.Warn("recording base upload", "err", err)
		return
	}
	s.cfg.Log.Info("base bundle cached", "sha", vol.BaseSHA, "bytes", size)
}

// Identify reports which epoch and pod this sidecar serves.
func (s *Server) Identify(context.Context, *sidecarv1.IdentifyRequest) (*sidecarv1.IdentifyResponse, error) {
	resp := &sidecarv1.IdentifyResponse{WorkspaceUid: s.cfg.WorkspaceUID, Epoch: s.cfg.Epoch, PodUid: s.cfg.PodUID}
	if vol, ok, err := ReadVolume(s.cfg.Root); err == nil && ok {
		switch vol.State {
		case StateLive:
			resp.VolumeState = sidecarv1.VolumeState_VOLUME_STATE_LIVE
		case StateRestoring:
			resp.VolumeState = sidecarv1.VolumeState_VOLUME_STATE_RESTORING
		}
	}
	return resp, nil
}

func (s *Server) StartSnapshot(_ context.Context, r *sidecarv1.StartSnapshotRequest) (*sidecarv1.StartSnapshotResponse, error) {
	op, err := s.snaps.Start(r)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &sidecarv1.StartSnapshotResponse{Snapshot: op}, nil
}

func (s *Server) GetSnapshot(_ context.Context, r *sidecarv1.GetSnapshotRequest) (*sidecarv1.GetSnapshotResponse, error) {
	op, err := s.snaps.Get(r.GetAttemptId())
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.NotFound, "no such attempt")
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &sidecarv1.GetSnapshotResponse{Snapshot: op}, nil
}

// WatchProgress sends the restore milestones reached so far, then keeps the
// stream open (later milestones, such as dependencies, arrive on it).
func (s *Server) WatchProgress(_ *sidecarv1.WatchProgressRequest, stream sidecarv1.SidecarService_WatchProgressServer) error {
	vol, ok, err := ReadVolume(s.cfg.Root)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if ok && vol.State == StateLive && !vol.FilesReadyAt.IsZero() {
		if err := stream.Send(&sidecarv1.WatchProgressResponse{
			Stage: sidecarv1.Stage_STAGE_FILES_READY, At: timestamppb.New(vol.FilesReadyAt), Bytes: vol.RestoredBytes,
		}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}
