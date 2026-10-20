package workspace

import (
	"context"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// GRPCPods dials pods directly by IP, presenting tokens derived for the
// pod's workspace and epoch. Each call opens and closes its own connection:
// calls are rare (a handful per suspend or wake) and this keeps no state.
type GRPCPods struct{ Keys token.Keyring }

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

func dial(ip string, port int, tok string) (*grpc.ClientConn, error) {
	return grpc.NewClient(net.JoinHostPort(ip, strconv.Itoa(port)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(tok)))
}

func (g GRPCPods) Agent(ws *sbxv1.Workspace, pod Pod) Agent {
	return grpcAgent{ip: pod.IP, tok: g.Keys.Derive(token.Agent, string(ws.UID), pod.Epoch)}
}

func (g GRPCPods) Sidecar(ws *sbxv1.Workspace, pod Pod) Sidecar {
	return grpcSidecar{ip: pod.IP, tok: g.Keys.Derive(token.Sidecar, string(ws.UID), pod.Epoch)}
}

type grpcAgent struct{ ip, tok string }

func (a grpcAgent) Freeze(ctx context.Context, attempt string, lease time.Time) error {
	conn, err := dial(a.ip, podspec.AgentGRPCPort, a.tok)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := &agentv1.FreezeRequest{AttemptId: attempt}
	if !lease.IsZero() {
		req.LeaseUntil = timestamppb.New(lease)
	}
	_, err = agentv1.NewAgentServiceClient(conn).Freeze(ctx, req)
	return err
}

func (a grpcAgent) Thaw(ctx context.Context, attempt string) error {
	conn, err := dial(a.ip, podspec.AgentGRPCPort, a.tok)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = agentv1.NewAgentServiceClient(conn).Thaw(ctx, &agentv1.ThawRequest{AttemptId: attempt})
	return err
}

type grpcSidecar struct{ ip, tok string }

func (s grpcSidecar) client() (sidecarv1.SidecarServiceClient, func(), error) {
	conn, err := dial(s.ip, podspec.SidecarPort, s.tok)
	if err != nil {
		return nil, nil, err
	}
	return sidecarv1.NewSidecarServiceClient(conn), func() { _ = conn.Close() }, nil
}

func (s grpcSidecar) StartSnapshot(ctx context.Context, r *sidecarv1.StartSnapshotRequest) (*sidecarv1.Snapshot, error) {
	c, done, err := s.client()
	if err != nil {
		return nil, err
	}
	defer done()
	resp, err := c.StartSnapshot(ctx, r)
	return resp.GetSnapshot(), err
}

func (s grpcSidecar) GetSnapshot(ctx context.Context, attempt string) (*sidecarv1.Snapshot, error) {
	c, done, err := s.client()
	if err != nil {
		return nil, err
	}
	defer done()
	resp, err := c.GetSnapshot(ctx, &sidecarv1.GetSnapshotRequest{AttemptId: attempt})
	return resp.GetSnapshot(), err
}

func (s grpcSidecar) FilesReady(ctx context.Context) (time.Time, int64, error) {
	c, done, err := s.client()
	if err != nil {
		return time.Time{}, 0, err
	}
	defer done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.WatchProgress(ctx, &sidecarv1.WatchProgressRequest{})
	if err != nil {
		return time.Time{}, 0, err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return time.Time{}, 0, err
		}
		if ev.GetStage() == sidecarv1.Stage_STAGE_FILES_READY {
			return ev.GetAt().AsTime(), ev.GetBytes(), nil
		}
	}
}
