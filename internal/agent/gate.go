package agent

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"

	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
)

// FrozenTrailer is set on gRPC responses refused because the workload is
// frozen; FrozenHeader is its HTTP equivalent. The gateway uses them to tell
// "hold and retry after the wake" apart from a real failure.
const (
	FrozenTrailer = "sbx-frozen"
	FrozenHeader  = "X-Sbx-Frozen"
)

const processService = "/process.v1.ProcessService/"

// errFrozen is returned for calls refused during a freeze.
func errFrozen(ctx context.Context) error {
	_ = grpc.SetTrailer(ctx, metadata.Pairs(FrozenTrailer, "1"))
	return status.Error(codes.Unavailable, "sbx: workspace is frozen for a snapshot")
}

// unaryGate refuses exec calls while frozen and records activity and running
// commands for the rest.
func unaryGate(f *Freezer, a *Activity) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, processService) {
			return next(ctx, req)
		}
		if f.Frozen() {
			return nil, errFrozen(ctx)
		}
		a.Touch()
		if info.FullMethod == processv1.ProcessService_Execute_FullMethodName {
			if r, ok := req.(*processv1.ExecuteRequest); ok {
				done := a.StartCommand(strings.Join(r.GetConfig().GetCommand(), " "), signalsv1.CommandKind_COMMAND_KIND_EXEC)
				defer done()
			}
		}
		return next(ctx, req)
	}
}

// streamGate does the same for streaming Start calls, which also count as an
// open exec stream for as long as they run.
func streamGate(f *Freezer, a *Activity) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if !strings.HasPrefix(info.FullMethod, processService) {
			return next(srv, ss)
		}
		if f.Frozen() {
			return errFrozen(ss.Context())
		}
		a.Touch()
		closeStream := a.OpenStream(signalsv1.StreamKind_STREAM_KIND_EXEC)
		defer closeStream()
		w := &startWatcher{ServerStream: ss, activity: a}
		defer w.done()
		return next(srv, w)
	}
}

// startWatcher records the command named in a Start stream's request.
type startWatcher struct {
	grpc.ServerStream
	activity *Activity
	finish   func()
}

func (w *startWatcher) RecvMsg(m any) error {
	err := w.ServerStream.RecvMsg(m)
	if r, ok := m.(*processv1.StartRequest); ok && err == nil && w.finish == nil {
		w.finish = w.activity.StartCommand(strings.Join(r.GetConfig().GetCommand(), " "), signalsv1.CommandKind_COMMAND_KIND_EXEC)
	}
	return err
}

func (w *startWatcher) done() {
	if w.finish != nil {
		w.finish()
	}
}

// restGate refuses file writes while frozen, tracks writes in flight so a
// freeze can wait for them, and records activity. Reads and the health check
// pass through; health checks never count as activity.
func restGate(f *Freezer, a *Activity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			if r.URL.Path != "/v1/health" {
				a.Touch()
			}
			next.ServeHTTP(w, r)
			return
		}
		done := a.BeginWrite()
		defer done()
		// Check after registering the write, so a concurrent Freeze either
		// sees this write in flight or this check sees the freeze.
		if f.Frozen() {
			w.Header().Set(FrozenHeader, "1")
			http.Error(w, "sbx: workspace is frozen for a snapshot", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
