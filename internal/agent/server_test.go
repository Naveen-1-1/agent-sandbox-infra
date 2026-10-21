package agent_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

func TestMain(m *testing.M) {
	if os.Getenv("SBX_FAKE_EDITOR") == "1" {
		fakeEditor() // this binary, started by the agent as its "code-server"
		return
	}
	leaktest.VerifyTestMain(m)
}

// noProcs is an empty process table: the freeze gate is what's under test.
type noProcs struct{}

func (noProcs) List() ([]int, error)             { return nil, nil }
func (noProcs) Signal(int, syscall.Signal) error { return nil }

const tok = "agent-test-token"

type harness struct {
	conn     *grpc.ClientConn
	proc     processv1.ProcessServiceClient
	agent    agentv1.AgentServiceClient
	rest     string
	activity string // the activity endpoint
	root     string
	cancel   context.CancelFunc
}

func start(t *testing.T) *harness { return startWith(t, nil) }

// startWith runs an agent whose config edit adjusts.
func startWith(t *testing.T, edit func(*agent.Config)) *harness {
	t.Helper()
	root := t.TempDir()
	cfg := agent.Config{Root: root, TokenSHA256: token.SHA256(tok), Procs: noProcs{}}
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ActivityAddr = al.Addr().String()
	if edit != nil {
		edit(&cfg)
	}
	srv, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ServeListeners(ctx, gl, hl, al) }()

	conn, err := grpc.NewClient(gl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		conn: conn, proc: processv1.NewProcessServiceClient(conn), agent: agentv1.NewAgentServiceClient(conn),
		rest: "http://" + hl.Addr().String(), activity: "http://" + al.Addr().String() + "/v1/activity", root: root, cancel: cancel,
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		http.DefaultClient.CloseIdleConnections()
	})
	return h
}

func authed(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func run(t *testing.T, h *harness, argv ...string) (*processv1.ExecuteResponse, metadata.MD, error) {
	t.Helper()
	var trailer metadata.MD
	resp, err := h.proc.Execute(authed(t), &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: argv}}, grpc.Trailer(&trailer))
	return resp, trailer, err
}

func httpDo(t *testing.T, method, url string, body []byte, auth bool) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(authed(t), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

func TestExecRequiresTheAgentToken(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.proc.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"true"}}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated Execute: %v, want Unauthenticated", err)
	}
	if code, _ := httpDo(t, http.MethodGet, h.rest+"/v1/health", nil, false); code != http.StatusOK {
		t.Fatalf("health without a token: %d, want 200", code)
	}
}

func TestFreezeRefusesWritesAndThawRestores(t *testing.T) {
	h := start(t)
	resp, _, err := run(t, h, "echo", "hi")
	if err != nil || strings.TrimSpace(string(resp.GetStdout())) != "hi" {
		t.Fatalf("Execute before freeze: %q, %v", resp.GetStdout(), err)
	}

	if _, err := h.agent.Freeze(authed(t), &agentv1.FreezeRequest{AttemptId: "a1"}); err != nil {
		t.Fatal(err)
	}
	_, trailer, err := run(t, h, "touch", "x")
	if status.Code(err) != codes.Unavailable || trailer.Get(agent.FrozenTrailer) == nil {
		t.Fatalf("Execute while frozen: err %v, trailer %v; want Unavailable with %s", err, trailer, agent.FrozenTrailer)
	}
	code, hdr := httpDo(t, http.MethodPut, h.rest+"/v1/files/new.txt", []byte("data"), true)
	if code != http.StatusServiceUnavailable || hdr.Get(agent.FrozenHeader) != "1" {
		t.Fatalf("file write while frozen: %d %v, want 503 with %s", code, hdr, agent.FrozenHeader)
	}
	if err := os.WriteFile(filepath.Join(h.root, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := httpDo(t, http.MethodGet, h.rest+"/v1/files/existing.txt", nil, true); code != http.StatusOK {
		t.Fatalf("file read while frozen: %d, want 200", code)
	}
	sig, err := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if err != nil || !sig.GetFrozen() {
		t.Fatalf("GetSignals while frozen: frozen=%v err=%v", sig.GetFrozen(), err)
	}

	if _, err := h.agent.Thaw(authed(t), &agentv1.ThawRequest{AttemptId: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, h, "true"); err != nil {
		t.Fatalf("Execute after thaw: %v", err)
	}
	if code, _ := httpDo(t, http.MethodPut, h.rest+"/v1/files/new.txt", []byte("data"), true); code >= 300 {
		t.Fatalf("file write after thaw: %d", code)
	}
}

func TestSignalsReportRunningCommandsAndStreams(t *testing.T) {
	h := start(t)
	before, err := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(authed(t))
	stream, err := h.proc.Start(ctx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"sleep", "30"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil { // init event: the process is running
		t.Fatal(err)
	}
	during, err := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cmds := during.GetSignals().GetRunningCommands()
	if len(cmds) != 1 || cmds[0].GetCommand() != "sleep 30" || cmds[0].GetKind() != signalsv1.CommandKind_COMMAND_KIND_EXEC {
		t.Fatalf("running commands = %v, want [sleep 30 (exec)]", cmds)
	}
	if s := during.GetSignals().GetOpenStreams(); len(s) != 1 || s[0].GetKind() != signalsv1.StreamKind_STREAM_KIND_EXEC || s[0].GetCount() != 1 {
		t.Fatalf("open streams = %v, want one exec stream", s)
	}
	if during.GetSignals().GetLastActivity().AsTime().Before(before.GetSignals().GetLastActivity().AsTime()) {
		t.Fatal("starting a command did not count as activity")
	}

	cancel() // client goes away; upstream kills the process
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, err := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
		if err == nil && len(s.GetSignals().GetRunningCommands()) == 0 && len(s.GetSignals().GetOpenStreams()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("command and stream still reported after the client disconnected")
}
