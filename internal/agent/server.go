package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	sandboxd "sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
)

// Config configures the agent server.
type Config struct {
	// Root is the workspace directory; exec and file calls are confined to it.
	Root string
	// GRPCAddr serves upstream ProcessService and AgentService. Health is
	// checked over REST (/v1/health), which needs no token.
	GRPCAddr string
	// RESTAddr serves upstream's files API and /v1/health.
	RESTAddr string
	// TokenSHA256 is the SHA-256 of the agent token; every call but the
	// health checks must present the token. Nil disables auth (tests only).
	TokenSHA256 []byte
	// Procs lists and signals workload processes (default: /proc).
	Procs Procs
	// ActivityAddr serves the editor extension's activity reports
	// (default 127.0.0.1:9092). Keep it on loopback: it takes no token.
	ActivityAddr string
	// Editor, if set, runs code-server on first use behind /sbx/editor/.
	Editor *EditorConfig
	Log    *slog.Logger
}

// Server is a running agent.
type Server struct {
	cfg      Config
	activity *Activity
	freezer  *Freezer
	services *Services // declared by the controller
	internal *Services // ours: the editor
	editor   *editor
	terms    *terminals
	upstream *sandboxd.Server
	grpc     *grpc.Server
	http     *http.Server
	reports  *http.Server // activity reports
}

// New builds the agent: upstream sandboxd with our gate in front, plus
// AgentService on the same gRPC server.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Procs == nil {
		cfg.Procs = SystemProcs{}
	}
	up, err := sandboxd.New(sandboxd.Options{
		RootDir:         cfg.Root,
		AuthTokenSHA256: cfg.TokenSHA256,
		Log:             logr.FromSlogHandler(cfg.Log.Handler()),
	})
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, activity: NewActivity(nil), upstream: up}
	s.freezer = NewFreezer(cfg.Procs, s.activity, 1, os.Getpid())
	s.services = NewServices(cfg.Root, s.activity, cfg.Log)
	s.internal = NewServices(cfg.Root, s.activity, cfg.Log)
	s.terms = newTerminals(s.activity)
	if cfg.ActivityAddr == "" {
		s.cfg.ActivityAddr = "127.0.0.1:9092"
	}
	if cfg.Editor != nil {
		s.editor = &editor{cfg: *cfg.Editor, root: cfg.Root, services: s.internal,
			activity: "http://" + s.cfg.ActivityAddr + "/v1/activity"}
	}

	// Upstream's auth interceptors run first, then our gate.
	opts := append(up.GRPCServerOptions(),
		grpc.ChainUnaryInterceptor(unaryGate(s.freezer, s.activity)),
		grpc.ChainStreamInterceptor(streamGate(s.freezer, s.activity)),
	)
	s.grpc = grpc.NewServer(opts...)
	up.RegisterGRPC(s.grpc)
	agentv1.RegisterAgentServiceServer(s.grpc, &service{s: s})

	s.http = &http.Server{
		Handler:           s.proxyRoutes(restGate(s.freezer, s.activity, up.RESTHandler())),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.reports = &http.Server{Handler: s.activityHandler(), ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

// Activity exposes the activity tracker (for the editor activity endpoint).
func (s *Server) Activity() *Activity { return s.activity }

// Serve listens on the configured addresses until ctx ends, then stops
// running processes and shuts both servers down.
func (s *Server) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	gl, err := lc.Listen(ctx, "tcp", s.cfg.GRPCAddr)
	if err != nil {
		return err
	}
	hl, err := lc.Listen(ctx, "tcp", s.cfg.RESTAddr)
	if err != nil {
		_ = gl.Close()
		return err
	}
	al, err := lc.Listen(ctx, "tcp", s.cfg.ActivityAddr)
	if err != nil {
		_ = gl.Close()
		_ = hl.Close()
		return err
	}
	return s.ServeListeners(ctx, gl, hl, al)
}

// ServeListeners is Serve on listeners the caller opened (used by tests).
// activityL may be nil.
func (s *Server) ServeListeners(ctx context.Context, grpcL, restL, activityL net.Listener) error {
	errc := make(chan error, 3)
	go func() { errc <- s.grpc.Serve(grpcL) }()
	serveHTTP := func(srv *http.Server, l net.Listener) {
		err := srv.Serve(l)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}
	go serveHTTP(s.http, restL)
	pending := 2
	if activityL != nil {
		go serveHTTP(s.reports, activityL)
		pending++
	}
	s.upstream.SetReady(true)
	s.cfg.Log.Info("agent serving", "grpc", grpcL.Addr().String(), "rest", restL.Addr().String())

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errc:
		pending--
	}

	s.upstream.SetReady(false)
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A frozen workload can't act on SIGTERM; resume it so it can exit.
	_ = s.freezer.Thaw(s.freezer.currentAttempt())
	s.services.Close()
	s.internal.Close()
	s.upstream.ShutdownProcesses(shutdown)
	_ = s.http.Shutdown(shutdown)
	_ = s.reports.Shutdown(shutdown)
	stopped := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutdown.Done():
		s.grpc.Stop()
		<-stopped
	}
	for ; pending > 0; pending-- {
		if err := <-errc; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("agent: server failed: %w", firstErr)
	}
	return nil
}

func (f *Freezer) currentAttempt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempt
}

// service implements AgentService.
type service struct {
	agentv1.UnimplementedAgentServiceServer
	s *Server
}

func (v *service) Freeze(ctx context.Context, r *agentv1.FreezeRequest) (*agentv1.FreezeResponse, error) {
	if r.GetAttemptId() == "" {
		return nil, status.Error(codes.InvalidArgument, "attempt_id is required")
	}
	var lease time.Time
	if r.GetLeaseUntil() != nil {
		lease = r.GetLeaseUntil().AsTime()
	}
	n, err := v.s.freezer.Freeze(ctx, r.GetAttemptId(), lease)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.FreezeResponse{StoppedProcesses: int32(n)}, nil //nolint:gosec // process counts are small
}

func (v *service) Thaw(_ context.Context, r *agentv1.ThawRequest) (*agentv1.ThawResponse, error) {
	if err := v.s.freezer.Thaw(r.GetAttemptId()); err != nil {
		if errors.Is(err, ErrNotFrozen) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.ThawResponse{}, nil
}

func (v *service) GetSignals(context.Context, *agentv1.GetSignalsRequest) (*agentv1.GetSignalsResponse, error) {
	v.s.terms.prune()
	return &agentv1.GetSignalsResponse{Signals: v.s.activity.Signals(), Frozen: v.s.freezer.Frozen()}, nil
}

// StartService runs a declared service, or reports it if it already runs.
func (v *service) StartService(ctx context.Context, r *agentv1.StartServiceRequest) (*agentv1.StartServiceResponse, error) {
	if v.s.freezer.Frozen() {
		return nil, errFrozen(ctx)
	}
	st, err := v.s.services.Start(r.GetService())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &agentv1.StartServiceResponse{Status: st}, nil
}

// ListServices reports every service.
func (v *service) ListServices(context.Context, *agentv1.ListServicesRequest) (*agentv1.ListServicesResponse, error) {
	return &agentv1.ListServicesResponse{Services: v.s.services.List()}, nil
}

// StopService ends a service.
func (v *service) StopService(_ context.Context, r *agentv1.StopServiceRequest) (*agentv1.StopServiceResponse, error) {
	if err := v.s.services.Stop(r.GetName()); err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &agentv1.StopServiceResponse{}, nil
}
