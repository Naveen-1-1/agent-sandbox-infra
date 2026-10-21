package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/gateway"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/envtestx"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

var (
	k8s                  client.Client
	keys                 token.Keyring
	agentGRPC, agentREST int
	agentClient          agentv1.AgentServiceClient
)

const (
	adminToken = "admin-token"
	agentToken = "agent-token"
)

type noProcs struct{}

func (noProcs) List() ([]int, error)             { return nil, nil }
func (noProcs) Signal(int, syscall.Signal) error { return nil }

func TestMain(m *testing.M) {
	scheme, err := envtestx.NewScheme(clientgoscheme.AddToScheme, sbxv1.AddToScheme)
	var apiServer *envtestx.Env
	if err == nil {
		apiServer, err = envtestx.Start(envtestx.Options{Scheme: scheme})
		if err == nil {
			k8s = apiServer.Client
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	keys, _ = token.NewKeyring(bytes.Repeat([]byte{3}, 32))

	// The "pod": a real sbx-agent on localhost.
	root, _ := os.MkdirTemp("", "gw-agent")
	a, err := agent.New(agent.Config{Root: root, TokenSHA256: token.SHA256(agentToken), Procs: noProcs{}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gl, _ := net.Listen("tcp", "127.0.0.1:0")
	hl, _ := net.Listen("tcp", "127.0.0.1:0")
	agentGRPC, agentREST = gl.Addr().(*net.TCPAddr).Port, hl.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { _ = a.ServeListeners(ctx, gl, hl); close(served) }()
	conn, _ := grpc.NewClient(gl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	agentClient = agentv1.NewAgentServiceClient(conn)

	leaktest.VerifyTestMain(leaktest.WithTeardown(m, func() {
		_ = conn.Close()
		cancel()
		<-served
		http.DefaultClient.CloseIdleConnections()
		apiServer.Stop()
		_ = os.RemoveAll(root)
	}))
}

type env struct {
	srv    *gateway.Server
	http   *httptest.Server
	reg    *prometheus.Registry
	ns     string
	admin  *sbxapi.Client
	stop   func()
	wakes  map[string]int
	mu     sync.Mutex
	delay  time.Duration
	frozen bool // the fake controller thaws the agent on wake
}

// start runs a gateway and a fake controller that wakes workspaces whose
// spec.wake.seq moved, after delay (negative: never).
func start(t *testing.T, cfg gateway.Config, delay time.Duration) *env {
	t.Helper()
	cfg.Keys, cfg.AdminToken = keys, adminToken
	cfg.AgentGRPCPort, cfg.AgentRESTPort = agentGRPC, agentREST
	cfg.AgentToken = func(*sbxv1.Workspace) string { return agentToken }
	ns := fmt.Sprintf("gw-%d", time.Now().UnixNano())
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	cfg.WorkspaceNamespace = ns
	reg := prometheus.NewRegistry()
	srv := gateway.New(cfg, k8s, k8s, k8s, reg, nil)
	srv.SetSynced()
	hs := httptest.NewServer(srv.APIHandler())
	e := &env{srv: srv, http: hs, reg: reg, ns: ns, wakes: map[string]int{}, delay: delay,
		admin: &sbxapi.Client{BaseURL: hs.URL, Token: adminToken}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go e.fakeController(ctx, done)
	e.stop = func() { cancel(); <-done; hs.Close(); srv.Close() }
	t.Cleanup(e.stop)
	return e
}

func (e *env) fakeController(ctx context.Context, done chan struct{}) {
	defer close(done)
	pending := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
		list := &sbxv1.WorkspaceList{}
		if err := k8s.List(ctx, list, client.InNamespace(e.ns)); err != nil {
			continue
		}
		for i := range list.Items {
			ws := &list.Items[i]
			key := client.ObjectKeyFromObject(ws).String()
			p := ws.Status.Phase
			asleep := p == sbxv1.PhaseSuspended || p == sbxv1.PhaseSuspending || p == sbxv1.PhaseTearingDown
			if e.delay < 0 || ws.Spec.Wake.Seq <= ws.Status.ObservedWakeSeq || !asleep {
				continue
			}
			if _, ok := pending[key]; !ok {
				pending[key] = time.Now()
			}
			if time.Since(pending[key]) < e.delay {
				continue
			}
			delete(pending, key)
			if e.frozen {
				_, _ = agentClient.Thaw(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+agentToken), &agentv1.ThawRequest{AttemptId: "a1"})
			}
			ws.Status = sbxv1.WorkspaceStatus{
				Phase: sbxv1.PhaseRunning, Epoch: ws.Status.Epoch + 1, ObservedWakeSeq: ws.Spec.Wake.Seq,
				Pod: &sbxv1.PodStatus{Name: ws.Name, UID: "pod", IP: "127.0.0.1"},
			}
			if err := k8s.Status().Update(ctx, ws); err != nil {
				continue
			}
			e.mu.Lock()
			e.wakes[key]++
			e.mu.Unlock()
			e.srv.Notify(key)
		}
	}
}

func (e *env) workspace(t *testing.T, name string, phase sbxv1.Phase, mode sbxv1.Mode) (*sbxv1.Workspace, *sbxapi.Client) {
	t.Helper()
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: name},
		Spec:       sbxv1.WorkspaceSpec{Source: sbxv1.Source{RepoURL: "https://example.com/r.git"}, Mode: mode},
	}
	if err := k8s.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	ws.Status = sbxv1.WorkspaceStatus{Phase: phase, Epoch: 1}
	if phase == sbxv1.PhaseRunning || phase == sbxv1.PhaseSuspending {
		ws.Status.Pod = &sbxv1.PodStatus{Name: name, UID: "pod", IP: "127.0.0.1"}
	}
	if phase == sbxv1.PhaseSuspending {
		ws.Status.Suspend = &sbxv1.SuspendStatus{AttemptID: "a1", PodUID: "pod", StartedAt: metav1.Now()}
	}
	if err := k8s.Status().Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	tok := keys.Derive(token.Gateway, string(ws.UID), 0)
	return ws, &sbxapi.Client{BaseURL: e.http.URL, Token: tok}
}

func (e *env) seq(t *testing.T, ws *sbxv1.Workspace) int64 {
	t.Helper()
	cur := &sbxv1.Workspace{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(ws), cur); err != nil {
		t.Fatal(err)
	}
	return cur.Spec.Wake.Seq
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func echo(words ...string) sbxapi.ExecRequest {
	return sbxapi.ExecRequest{Command: append([]string{"echo"}, words...)}
}

func statusOf(err error) int {
	var se *sbxapi.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

func TestAuthentication(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	ws, own := e.workspace(t, "a", sbxv1.PhaseRunning, "")
	_, other := e.workspace(t, "b", sbxv1.PhaseRunning, "")

	if _, err := own.Get(ctx(t), e.ns, ws.Name); err != nil {
		t.Fatalf("own token: %v", err)
	}
	if _, err := e.admin.Get(ctx(t), e.ns, ws.Name); err != nil {
		t.Fatalf("admin token: %v", err)
	}
	if _, err := other.Exec(ctx(t), e.ns, ws.Name, echo("x")); statusOf(err) != http.StatusUnauthorized {
		t.Errorf("another workspace's token: %v, want 401", err)
	}
	anon := &sbxapi.Client{BaseURL: e.http.URL}
	if _, err := anon.Get(ctx(t), e.ns, ws.Name); statusOf(err) != http.StatusUnauthorized {
		t.Errorf("no token: %v, want 401", err)
	}
	if _, err := own.List(ctx(t)); statusOf(err) != http.StatusUnauthorized {
		t.Errorf("listing with a workspace token: %v, want 401", err)
	}
	if _, err := own.Get(ctx(t), e.ns, "missing"); statusOf(err) != http.StatusUnauthorized {
		t.Errorf("a missing workspace must look the same as a wrong token to non-admins: %v", err)
	}
}

func TestCreateListDelete(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	resp, err := e.admin.Create(ctx(t), sbxapi.CreateRequest{RepoURL: "https://example.com/repo.git"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.Workspace.Namespace != e.ns || resp.Workspace.Name == "" {
		t.Fatalf("create = %+v", resp)
	}
	own := &sbxapi.Client{BaseURL: e.http.URL, Token: resp.Token}
	if _, err := own.Get(ctx(t), e.ns, resp.Workspace.Name); err != nil {
		t.Fatalf("returned token rejected: %v", err)
	}
	list, err := e.admin.List(ctx(t))
	if err != nil || len(list) == 0 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if err := e.admin.Delete(ctx(t), e.ns, resp.Workspace.Name); err != nil {
		t.Fatal(err)
	}
}

func TestExecOnARunningWorkspace(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	ws, c := e.workspace(t, "run", sbxv1.PhaseRunning, "")
	out, err := c.Exec(ctx(t), e.ns, ws.Name, echo("hello"))
	if err != nil || out.Stdout != "hello\n" || out.WaitedMillis != 0 {
		t.Fatalf("exec = %+v, %v", out, err)
	}
	if err := c.WriteFile(ctx(t), e.ns, ws.Name, "notes/a.txt", []byte("content")); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ReadFile(ctx(t), e.ns, ws.Name, "notes/a.txt"); err != nil || string(got) != "content" {
		t.Fatalf("read back %q, %v", got, err)
	}
}

func TestConcurrentRequestsAndHintsWakeOnce(t *testing.T) {
	e := start(t, gateway.Config{}, 150*time.Millisecond)
	ws, c := e.workspace(t, "storm", sbxv1.PhaseSuspended, "")

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				errs <- c.WakeHint(ctx(t), e.ns, ws.Name)
				return
			}
			out, err := c.Exec(ctx(t), e.ns, ws.Name, echo("ok"))
			if err == nil && out.Stdout != "ok\n" {
				err = fmt.Errorf("stdout %q", out.Stdout)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := e.seq(t, ws); got != 1 {
		t.Errorf("spec.wake.seq = %d after 40 requests and 20 hints, want exactly one bump", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := e.wakes[client.ObjectKeyFromObject(ws).String()]; n != 1 {
		t.Errorf("%d wakes, want 1", n)
	}
}

func TestBoundsTimeoutAndOff(t *testing.T) {
	e := start(t, gateway.Config{HoldPerWorkspace: 3, HoldTimeout: 300 * time.Millisecond}, -1)
	ws, c := e.workspace(t, "slow", sbxv1.PhaseSuspended, "")

	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Exec(ctx(t), e.ns, ws.Name, echo("x"))
			codes <- statusOf(err)
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[http.StatusServiceUnavailable] != 3 || counts[http.StatusGatewayTimeout] != 3 {
		t.Errorf("status codes %v, want 3x503 (queue full) and 3x504 (wake timeout)", counts)
	}

	off, oc := e.workspace(t, "off", sbxv1.PhaseSuspended, sbxv1.ModeOff)
	if _, err := oc.Exec(ctx(t), e.ns, off.Name, echo("x")); statusOf(err) != http.StatusConflict {
		t.Errorf("exec on an Off workspace: %v, want 409", err)
	}
	if e.seq(t, off) != 0 {
		t.Error("an Off workspace was asked to wake")
	}
}

func TestStatusAndHealthNeverWake(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	ws, c := e.workspace(t, "sleepy", sbxv1.PhaseSuspended, "")
	if v, err := c.Get(ctx(t), e.ns, ws.Name); err != nil || v.Phase != string(sbxv1.PhaseSuspended) {
		t.Fatalf("get = %+v, %v", v, err)
	}
	admin := httptest.NewServer(e.srv.AdminHandler(e.reg))
	defer admin.Close()
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := http.Get(admin.URL + p)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", p, resp, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	time.Sleep(50 * time.Millisecond)
	if e.seq(t, ws) != 0 {
		t.Fatal("a status or health request woke the workspace")
	}
}

func TestWakeHintStartsTheWakeWithoutWaiting(t *testing.T) {
	e := start(t, gateway.Config{}, 200*time.Millisecond)
	ws, c := e.workspace(t, "hinted", sbxv1.PhaseSuspended, "")
	began := time.Now()
	if err := c.WakeHint(ctx(t), e.ns, ws.Name); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 150*time.Millisecond {
		t.Fatal("wake-hint waited for the wake")
	}
	if e.seq(t, ws) != 1 {
		t.Fatal("wake-hint did not request a wake")
	}
	time.Sleep(400 * time.Millisecond) // the "LLM" thinks; the wake finishes meanwhile
	out, err := c.Exec(ctx(t), e.ns, ws.Name, echo("hidden"))
	if err != nil || out.WaitedMillis != 0 {
		t.Fatalf("exec after the hint: %+v, %v; want no visible wait", out, err)
	}
	if n := testutil.CollectAndCount(e.reg, "sbx_wake_hint_lead_seconds"); n != 1 {
		t.Errorf("hint lead observed %d times, want 1", n)
	}
}

func TestRequestDuringSuspendAbortsIt(t *testing.T) {
	e := start(t, gateway.Config{}, 50*time.Millisecond)
	e.frozen = true
	ws, c := e.workspace(t, "suspending", sbxv1.PhaseSuspending, "")
	actx := metadata.AppendToOutgoingContext(ctx(t), "authorization", "Bearer "+agentToken)
	if _, err := agentClient.Freeze(actx, &agentv1.FreezeRequest{AttemptId: "a1"}); err != nil {
		t.Fatal(err)
	}
	out, err := c.Exec(ctx(t), e.ns, ws.Name, echo("after-abort"))
	if err != nil || out.Stdout != "after-abort\n" {
		t.Fatalf("exec during a suspend: %+v, %v", out, err)
	}
	if e.seq(t, ws) != 1 {
		t.Error("the request did not ask for a wake (which is what aborts the suspend)")
	}
}

func TestSignalsReportTraffic(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	ws, c := e.workspace(t, "busy", sbxv1.PhaseRunning, "")
	if _, err := c.Exec(ctx(t), e.ns, ws.Name, echo("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.WakeHint(ctx(t), e.ns, ws.Name); err != nil {
		t.Fatal(err)
	}
	if err := c.TurnEnded(ctx(t), e.ns, ws.Name); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ws).String()
	resp, err := e.srv.Signals().GetWorkspaceSignals(ctx(t), &gatewayv1.GetWorkspaceSignalsRequest{Workspaces: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	s := resp.GetSignals()[key]
	if s.GetLastActivity() == nil || s.GetLastTurnStarted() == nil || s.GetLastTurnEnded() == nil {
		t.Fatalf("signals = %v", s)
	}
}
