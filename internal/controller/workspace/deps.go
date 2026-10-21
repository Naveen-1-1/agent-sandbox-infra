package workspace

import (
	"context"
	"time"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// Agent is the part of a pod's sbx-agent the controller drives.
type Agent interface {
	// Freeze stops the workload. A zero leaseUntil holds the freeze until
	// the pod is deleted.
	Freeze(ctx context.Context, attemptID string, leaseUntil time.Time) error
	Thaw(ctx context.Context, attemptID string) error
	// Signals reports in-pod activity for the sleep policy.
	Signals(ctx context.Context) (*signalsv1.Signals, error)
}

// GatewaySignals is what the gateway replicas know about a workspace's
// traffic: turns, held requests and open streams.
type GatewaySignals interface {
	// Signals returns the merged view for "namespace/name"; ok is false when
	// some replica's view is missing or stale.
	Signals(key string) (s policy.Signals, ok bool)
}

// Sidecar is the part of a pod's sbx-sidecar the controller drives.
type Sidecar interface {
	StartSnapshot(ctx context.Context, r *sidecarv1.StartSnapshotRequest) (*sidecarv1.Snapshot, error)
	GetSnapshot(ctx context.Context, attemptID string) (*sidecarv1.Snapshot, error)
	// FilesReady reports when this epoch's files were restored and how many
	// bytes were downloaded.
	FilesReady(ctx context.Context) (time.Time, int64, error)
}

// Pod identifies the pod serving an epoch.
type Pod struct {
	IP    string
	Epoch int64
}

// PodClients connects to a workspace pod's agent and sidecar with that
// epoch's tokens.
type PodClients interface {
	Agent(ws *sbxv1.Workspace, pod Pod) Agent
	Sidecar(ws *sbxv1.Workspace, pod Pod) Sidecar
}

// Base is an upstream commit resolved from a workspace's source.
type Base struct {
	SHA    string
	Branch string
}

// Inspector resolves a workspace's source to a base commit.
type Inspector interface {
	Resolve(ctx context.Context, src sbxv1.Source) (Base, error)
}

// PermanentError marks a failure retrying won't fix (e.g. no such branch).
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }
