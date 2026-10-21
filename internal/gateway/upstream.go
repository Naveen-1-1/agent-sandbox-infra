package gateway

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

type bearerCreds string

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearerCreds) RequireTransportSecurity() bool { return false }

// execute runs a command through the pod's agent. frozen reports that the
// agent refused it because a snapshot is in progress; nothing ran.
func (s *Server) execute(ctx context.Context, ws *sbxv1.Workspace, r sbxapi.ExecRequest) (*processv1.ExecuteResponse, bool, error) {
	conn, err := grpc.NewClient(net.JoinHostPort(ws.Status.Pod.IP, strconv.Itoa(s.cfg.AgentGRPCPort)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerCreds(s.cfg.AgentToken(ws))))
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()

	timeout := s.cfg.ExecTimeout
	if r.TimeoutSeconds > 0 {
		timeout = time.Duration(r.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg := &processv1.ProcessConfig{Command: r.Command, EnvVars: r.Env}
	if r.Cwd != "" {
		cfg.Cwd = &r.Cwd
	}
	var trailer metadata.MD
	resp, err := processv1.NewProcessServiceClient(conn).Execute(ctx, &processv1.ExecuteRequest{Config: cfg}, grpc.Trailer(&trailer))
	if err != nil && len(trailer.Get(agent.FrozenTrailer)) > 0 {
		return nil, true, nil
	}
	return resp, false, err
}

// forwardFile proxies one files-API request to the pod's agent. A frozen
// refusal is reported, not written, so the caller can retry after a wake.
func (s *Server) forwardFile(ctx context.Context, ws *sbxv1.Workspace, method, path, rawQuery, contentType string, body []byte) (*http.Response, bool, error) {
	url := "http://" + net.JoinHostPort(ws.Status.Pod.IP, strconv.Itoa(s.cfg.AgentRESTPort)) + "/v1/files/" + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	// The host is the pod IP from controller-written status, never from the
	// client; the client's path is confined to the workspace by sandboxd.
	req, err := http.NewRequestWithContext(ctx, method, url, rd) //nolint:gosec // see above
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.AgentToken(ws))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.podHTTP.Do(req) //nolint:gosec // host comes from workspace status
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get(agent.FrozenHeader) != "" {
		_ = resp.Body.Close()
		return nil, true, nil
	}
	return resp, false, nil
}
