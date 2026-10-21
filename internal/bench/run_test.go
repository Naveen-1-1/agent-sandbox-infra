package bench

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
)

// clock is a fake clock that only moves when someone sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

// fakeGateway plays one workspace's life: provisioning, an install, and
// suspends and wakes that take a fixed time on the fake clock. A command
// that wakes it is held 2s; one after a hint and 10s of thinking isn't.
type fakeGateway struct {
	clock   *clock
	ws      *sbxapi.Workspace
	files   map[string][]byte
	gets    int
	hintAt  *time.Time
	existed bool
}

func (g *fakeGateway) Create(_ context.Context, r sbxapi.CreateRequest) (*sbxapi.CreateResponse, error) {
	if r.Policy == nil || r.Policy.Name != "never" || r.Ref != strings.Repeat("c", 40) {
		return nil, errors.New("want policy never and the pinned commit")
	}
	g.ws = &sbxapi.Workspace{Namespace: "ws", Name: r.Name, Phase: "Provisioning"}
	g.files = map[string][]byte{"README.md": []byte("# repo\n")}
	return &sbxapi.CreateResponse{Workspace: *g.ws}, nil
}

func (g *fakeGateway) List(context.Context) ([]sbxapi.Workspace, error) {
	if g.existed {
		return []sbxapi.Workspace{{Name: "other"}}, nil
	}
	return nil, nil
}

func (g *fakeGateway) Get(context.Context, string, string) (*sbxapi.Workspace, error) {
	if g.ws == nil {
		return nil, &sbxapi.StatusError{Status: 404}
	}
	g.gets++
	now := g.clock.Now()
	switch g.ws.Phase {
	case "Provisioning":
		if g.gets > 3 {
			req := now.Add(-3 * time.Second)
			g.ws.Phase, g.ws.Epoch = "Running", 1
			g.ws.Timeline = &sbxapi.Timeline{RequestedAt: &req, FilesReadyAt: ptr(req.Add(time.Second)), ReadyAt: ptr(req.Add(2 * time.Second))}
			g.ws.Deps = &sbxapi.Deps{Profile: "npm", Pending: true}
		}
	case "Running":
		if g.ws.Deps.Pending && g.ws.Epoch == 1 {
			g.ws.Deps = &sbxapi.Deps{Profile: "npm", Source: "Install", Ready: true}
			g.ws.Timeline.DepsReadyAt = ptr(g.ws.Timeline.ReadyAt.Add(30 * time.Second))
		}
	case "Suspending":
		g.ws.Phase = "Suspended"
		g.ws.LastSnapshot = &sbxapi.Snapshot{Bytes: 1000 + g.ws.Epoch}
	}
	cp := *g.ws
	return &cp, nil
}

func (g *fakeGateway) Delete(context.Context, string, string) error { g.ws = nil; return nil }

func (g *fakeGateway) Suspend(context.Context, string, string) error {
	if g.ws.Phase != "Running" {
		return errors.New("not running")
	}
	g.ws.Phase = "Suspending"
	return nil
}

func (g *fakeGateway) WakeHint(context.Context, string, string) error {
	now := g.clock.Now()
	g.hintAt = &now
	return nil
}

func (g *fakeGateway) Exec(_ context.Context, _, _ string, r sbxapi.ExecRequest) (*sbxapi.ExecResponse, error) {
	script := r.Command[len(r.Command)-1]
	if script == agentScript {
		sep := "\n" + sectionSep + "\n"
		return &sbxapi.ExecResponse{Stdout: sep + stat("sbx-agent", 10) + sep + status(5000) + sep + stat("sbx-agent", 90) + sep + status(30000)}, nil
	}
	out := &sbxapi.ExecResponse{Stdout: "ok"}
	if g.ws.Phase == "Suspended" {
		req := g.clock.Now()
		if g.hintAt != nil {
			req = *g.hintAt
		}
		g.hintAt = nil
		g.ws.Phase, g.ws.Epoch = "Running", g.ws.Epoch+1
		g.ws.Timeline = &sbxapi.Timeline{RequestedAt: &req, FilesReadyAt: ptr(req.Add(time.Second)),
			ReadyAt: ptr(req.Add(2 * time.Second)), DepsReadyAt: ptr(req.Add(3 * time.Second))}
		g.ws.Deps = &sbxapi.Deps{Profile: "npm", Source: "Cache", Ready: true, Key: "k1", Bytes: 2048}
		if waited := req.Add(3 * time.Second).Sub(g.clock.Now()); waited > 0 {
			out.WaitedMillis = waited.Milliseconds()
		}
	}
	return out, nil
}

func (g *fakeGateway) ReadFile(_ context.Context, _, _, file string) ([]byte, error) {
	return g.files[file], nil
}

func (g *fakeGateway) WriteFile(_ context.Context, _, _, file string, data []byte) error {
	g.files[file] = data
	return nil
}

// fakeCluster's counters step once per read (see pageAt).
type fakeCluster struct{ reads int }

func (c *fakeCluster) ControllerMetrics(context.Context) ([]string, error) {
	c.reads++
	return []string{pageAt(c.reads)}, nil
}

func (c *fakeCluster) Sidecar(_ context.Context, _, _, script string) (string, error) {
	if script != sidecarScript {
		return "", errors.New("unexpected script")
	}
	sep := "\n" + sectionSep + "\n"
	return stat("sbx-sidecar", 230) + sep + status(40960) + sep + "104857600", nil
}

func (c *fakeCluster) Platform(context.Context) (Platform, error) {
	return Platform{Kubernetes: "v1.35.0", Nodes: []Node{{Name: "n1", Arch: "arm64"}}}, nil
}

func ptr[T any](v T) *T { return &v }

func testRepo() Repo {
	return Repo{Name: "app", URL: "https://example.com/app.git", Commit: strings.Repeat("c", 40),
		Profile: "npm", Image: "node", Command: "npm test", Edit: "README.md"}
}

func runner(g *fakeGateway, c *fakeCluster, clk *clock) *Runner {
	return &Runner{Gateway: g, Cluster: c, Out: io.Discard, Now: clk.Now, Sleep: clk.Sleep,
		Options: Options{Env: "test", Cycles: 4, Thinking: 10 * time.Second}}
}

func TestRunMeasuresColdStartAndCycles(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	g, c := &fakeGateway{clock: clk}, &fakeCluster{}
	res, err := runner(g, c, clk).Run(context.Background(), &Config{Repos: []Repo{testRepo()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repos) != 1 || res.Platform.Kubernetes != "v1.35.0" {
		t.Fatalf("result = %+v", res)
	}
	r := res.Repos[0]
	if r.FirstRun.DepsSource != "Install" || r.FirstRun.DepsInstallSeconds != 30 || r.FirstRun.ServingSeconds != 2 ||
		r.FirstRun.ProvisionSeconds <= 0 || r.FirstRun.CommandOutput != "ok" {
		t.Errorf("first run = %+v", r.FirstRun)
	}
	if r.FirstSuspend.N != 0 || r.FirstSuspend.DepsSource != "Cache" || r.DepArchiveBytes != 2048 || r.SandboxUsdPerHour != 0.0123 {
		t.Errorf("cycle 0 = %+v, archive %d, $/h %v", r.FirstSuspend, r.DepArchiveBytes, r.SandboxUsdPerHour)
	}
	var modes []bool
	for _, cy := range r.Cycles {
		modes = append(modes, cy.Hinted)
		if cy.DepsSource != "Cache" || cy.FilesReadySeconds != 1 || cy.ServingSeconds != 2 || cy.DepsReadySeconds != 3 {
			t.Errorf("cycle %d stages = %+v", cy.N, cy)
		}
		if cy.S3.PutClass != 4 || cy.S3.GetClass != 5 {
			t.Errorf("cycle %d S3 = %+v, want 4 put and 5 get per counter step", cy.N, cy.S3)
		}
		if cy.Sidecar.CPUSeconds != 2.3 || cy.Agent.CPUSeconds != 1 || cy.Agent.PeakRSSBytes != 30000*1024 {
			t.Errorf("cycle %d usage = %+v / %+v", cy.N, cy.Sidecar, cy.Agent)
		}
		want := 3.0 // a request wake: held until dependencies, 3s
		if cy.Hinted {
			want = 0 // 10s of thinking covered it
		}
		if cy.AgentWaitSeconds != want {
			t.Errorf("cycle %d (hinted %v) waited %v, want %v", cy.N, cy.Hinted, cy.AgentWaitSeconds, want)
		}
	}
	if !slices.Equal(modes, []bool{true, false, true, false}) {
		t.Errorf("modes = %v, want alternating from hinted", modes)
	}
	s := r.Summary
	if s.SuspendSeconds.N != 4 || s.CacheHits != 4 || s.AgentWaitSeconds["hinted"].Avg != 0 || s.AgentWaitSeconds["unhinted"].Avg != 3 {
		t.Errorf("summary = %+v", s)
	}
	if m := res.CostSim.Mean; m.WakeServeSeconds != 2 || m.WakeDepsSeconds != 3 || m.PutRequestsPerCycle != 4 || m.DepArchiveBytes != 2048 {
		t.Errorf("cost sim inputs = %+v", m)
	}
	if !strings.Contains(string(g.files["README.md"]), "sbx bench cycle 4") {
		t.Errorf("README.md = %q; each cycle edits it", g.files["README.md"])
	}
	if g.ws != nil {
		t.Error("the workspace was left behind")
	}
}

func TestRunNeedsThePlatformToItself(t *testing.T) {
	clk := &clock{}
	g := &fakeGateway{clock: clk, existed: true}
	if _, err := runner(g, &fakeCluster{}, clk).Run(context.Background(), &Config{Repos: []Repo{testRepo()}}); err == nil {
		t.Error("ran next to another workspace")
	}
}

func TestFreshRunWipesOnlyThatRepositorysCaches(t *testing.T) {
	ctx := context.Background()
	mem := objstore.NewMem()
	repo := testRepo()
	keep := []string{snapstore.BaseBundleKey("https://example.com/other.git", repo.Commit), snapstore.DepsKey("pip", "x"), "ws/u/manifest.json"}
	drop := []string{snapstore.BaseBundleKey(repo.URL, repo.Commit), snapstore.DepsKey("npm", "old")}
	for _, k := range append(slices.Clone(keep), drop...) {
		if _, err := mem.Put(ctx, k, strings.NewReader("x"), 1, objstore.Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{Store: mem, Out: io.Discard}
	if err := r.wipe(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for _, k := range keep {
		if _, err := mem.Head(ctx, k); err != nil {
			t.Errorf("%s was wiped", k)
		}
	}
	for _, k := range drop {
		if _, err := mem.Head(ctx, k); !errors.Is(err, objstore.ErrNotFound) {
			t.Errorf("%s survived", k)
		}
	}
	if _, err := (&Runner{Options: Options{Fresh: true}}).Run(ctx, &Config{}); err == nil {
		t.Error("a fresh run without object storage was accepted")
	}
}
