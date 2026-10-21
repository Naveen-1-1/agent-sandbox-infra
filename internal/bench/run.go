package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
)

// Gateway is the part of the gateway API the bench uses; *sbxapi.Client
// implements it.
type Gateway interface {
	Create(ctx context.Context, r sbxapi.CreateRequest) (*sbxapi.CreateResponse, error)
	List(ctx context.Context) ([]sbxapi.Workspace, error)
	Get(ctx context.Context, ns, name string) (*sbxapi.Workspace, error)
	Delete(ctx context.Context, ns, name string) error
	Exec(ctx context.Context, ns, name string, r sbxapi.ExecRequest) (*sbxapi.ExecResponse, error)
	WakeHint(ctx context.Context, ns, name string) error
	Suspend(ctx context.Context, ns, name string) error
	ReadFile(ctx context.Context, ns, name, file string) ([]byte, error)
	WriteFile(ctx context.Context, ns, name, file string, data []byte) error
}

// Cluster reads what the gateway API doesn't show.
type Cluster interface {
	// ControllerMetrics returns each controller replica's /metrics page.
	ControllerMetrics(ctx context.Context) ([]string, error)
	// Sidecar runs a shell script in a workspace pod's sidecar container.
	Sidecar(ctx context.Context, ns, pod, script string) (string, error)
	// Platform describes the cluster.
	Platform(ctx context.Context) (Platform, error)
}

// Options shape a run.
type Options struct {
	Env      string        // where it runs: kind, aws
	Cycles   int           // measured suspend/wake cycles per repository
	Thinking time.Duration // the simulated LLM's thinking after a wake-hint
	Poll     time.Duration // how often status is read while waiting
	// Fresh wipes each repository's cached base and dependency archives
	// first, so its first run is a cold miss. It needs Runner.Store.
	Fresh bool
	Keep  bool // keep the workspaces afterwards
}

// Runner runs the bench.
type Runner struct {
	Gateway Gateway
	Cluster Cluster
	// Store is the platform's object storage, for wiping caches and sizing
	// dependency archives; nil skips both.
	Store objstore.Store
	Out   io.Writer
	Options

	Now   func() time.Time                                 // default time.Now
	Sleep func(ctx context.Context, d time.Duration) error // default a timer
}

// Run measures every repository in turn.
func (r *Runner) Run(ctx context.Context, cfg *Config) (*Result, error) {
	r.defaults()
	if r.Fresh && r.Store == nil {
		return nil, errors.New("bench: a fresh run needs access to object storage")
	}
	if list, err := r.Gateway.List(ctx); err != nil {
		return nil, err
	} else if len(list) > 0 {
		return nil, fmt.Errorf("bench: %d workspaces exist; the bench needs the platform to itself", len(list))
	}
	res := &Result{Env: r.Env, StartedAt: r.Now().UTC()}
	res.Options.Cycles, res.Options.ThinkingSeconds = r.Cycles, r.Thinking.Seconds()
	p, err := r.Cluster.Platform(ctx)
	if err != nil {
		return nil, err
	}
	res.Platform = p
	for _, repo := range cfg.Repos {
		rr, err := r.repo(ctx, repo)
		if rr != nil {
			res.Repos = append(res.Repos, *rr)
		}
		if err != nil {
			return res, fmt.Errorf("%s: %w", repo.Name, err)
		}
	}
	res.FinishedAt = r.Now().UTC()
	res.CostSim = costSim(res.Repos)
	return res, nil
}

func (r *Runner) defaults() {
	if r.Out == nil {
		r.Out = io.Discard
	}
	if r.Cycles <= 0 {
		r.Cycles = 10
	}
	if r.Poll <= 0 {
		r.Poll = 100 * time.Millisecond
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Sleep == nil {
		r.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
}

func (r *Runner) repo(ctx context.Context, repo Repo) (*RepoResult, error) {
	res := &RepoResult{Repo: repo}
	r.say("%s: %s @ %.12s (%s)", repo.Name, repo.URL, repo.Commit, repo.Image)
	if r.Fresh {
		if err := r.wipe(ctx, repo); err != nil {
			return nil, err
		}
	}

	start := r.Now()
	created, err := r.Gateway.Create(ctx, sbxapi.CreateRequest{
		Name: "bench-" + repo.Name, RepoURL: repo.URL, Ref: repo.Commit, Image: repo.Image,
		Policy: &sbxapi.PolicyRef{Name: "never"}, // the bench decides when it sleeps
	})
	if err != nil {
		return nil, err
	}
	ns, name := created.Workspace.Namespace, created.Workspace.Name
	res.Workspace = ns + "/" + name
	if !r.Keep {
		defer r.remove(context.WithoutCancel(ctx), ns, name)
	}

	if _, err := r.waitPhase(ctx, ns, name, "Running", 15*time.Minute); err != nil {
		return res, err
	}
	res.FirstRun.ProvisionSeconds = r.Now().Sub(start).Seconds()
	ws, err := r.waitDeps(ctx, ns, name, 20*time.Minute)
	if err != nil {
		return res, err
	}
	if ws.Deps == nil || ws.Deps.Profile != repo.Profile || !ws.Deps.Ready {
		return res, fmt.Errorf("dependencies %+v, want profile %s ready", ws.Deps, repo.Profile)
	}
	if tl := ws.Timeline; tl != nil && tl.RequestedAt != nil {
		res.FirstRun.FilesReadySeconds = between(tl.RequestedAt, tl.FilesReadyAt)
		res.FirstRun.ServingSeconds = between(tl.RequestedAt, tl.ReadyAt)
		res.FirstRun.DepsInstallSeconds = between(tl.ReadyAt, tl.DepsReadyAt)
	}
	res.FirstRun.DepsSource = ws.Deps.Source
	sent := r.Now()
	first, err := r.command(ctx, ns, name, repo.Command)
	if err != nil {
		return res, err
	}
	res.FirstRun.CommandSeconds = r.Now().Sub(sent).Seconds()
	res.FirstRun.CommandOutput = tail(first.Stdout+first.Stderr, 400)
	r.say("  first run: up in %s, dependencies (%s) %s later, %q took %s", secs(res.FirstRun.ProvisionSeconds),
		res.FirstRun.DepsSource, secs(res.FirstRun.DepsInstallSeconds), repo.Command, secs(res.FirstRun.CommandSeconds))
	if c, err := r.counters(ctx); err == nil {
		res.SandboxUsdPerHour = c.RunningUsdPerHour
	}

	// Cycle 0 is the first suspend, which uploads the dependency archive,
	// and the first wake that restores it: unlike the others, so apart.
	epoch := ws.Epoch
	if res.FirstSuspend, epoch, err = r.cycle(ctx, repo, ns, name, 0, false, epoch); err != nil {
		return res, err
	}
	if res.DepArchiveBytes, err = r.archiveBytes(ctx, ns, name, repo.Profile); err != nil {
		return res, err
	}
	for i := 1; i <= r.Cycles; i++ {
		var c Cycle
		if c, epoch, err = r.cycle(ctx, repo, ns, name, i, i%2 == 1, epoch); err != nil {
			return res, err
		}
		res.Cycles = append(res.Cycles, c)
	}
	res.Summary = summarize(res.Cycles)
	return res, nil
}

// cycle edits a file, suspends the workspace and wakes it with the
// dependency command, after a wake-hint and the thinking time if hinted.
func (r *Runner) cycle(ctx context.Context, repo Repo, ns, name string, n int, hinted bool, epoch int64) (Cycle, int64, error) {
	c := Cycle{N: n, Hinted: hinted}
	data, err := r.Gateway.ReadFile(ctx, ns, name, repo.Edit)
	if err != nil {
		return c, epoch, fmt.Errorf("reading %s: %w", repo.Edit, err)
	}
	data = append(data, fmt.Sprintf("\nsbx bench cycle %d\n", n)...)
	if err := r.Gateway.WriteFile(ctx, ns, name, repo.Edit, data); err != nil {
		return c, epoch, fmt.Errorf("editing %s: %w", repo.Edit, err)
	}
	before, err := r.counters(ctx)
	if err != nil {
		return c, epoch, err
	}

	start := r.Now()
	if err := r.Gateway.Suspend(ctx, ns, name); err != nil {
		return c, epoch, err
	}
	ws, err := r.waitPhase(ctx, ns, name, "Suspended", 5*time.Minute)
	if err != nil {
		return c, epoch, err
	}
	c.SuspendSeconds = r.Now().Sub(start).Seconds()
	if ws.LastSnapshot != nil {
		c.BundleBytes = ws.LastSnapshot.Bytes
	}

	if hinted {
		if err := r.Gateway.WakeHint(ctx, ns, name); err != nil {
			return c, epoch, err
		}
		if err := r.Sleep(ctx, r.Thinking); err != nil {
			return c, epoch, err
		}
	}
	sent := r.Now()
	out, err := r.command(ctx, ns, name, repo.Command)
	if err != nil {
		return c, epoch, err
	}
	c.CommandSeconds = r.Now().Sub(sent).Seconds()
	c.AgentWaitSeconds = float64(out.WaitedMillis+out.DepsWaitedMillis) / 1000
	if ws, err = r.waitDeps(ctx, ns, name, 10*time.Minute); err != nil {
		return c, epoch, err
	}
	if ws.Epoch <= epoch {
		return c, epoch, fmt.Errorf("cycle %d: still epoch %d after the wake", n, ws.Epoch)
	}
	if tl := ws.Timeline; tl != nil && tl.RequestedAt != nil {
		c.FilesReadySeconds = between(tl.RequestedAt, tl.FilesReadyAt)
		c.ServingSeconds = between(tl.RequestedAt, tl.ReadyAt)
		c.DepsReadySeconds = between(tl.RequestedAt, tl.DepsReadyAt)
	}
	if ws.Deps != nil {
		c.DepsSource = ws.Deps.Source
	}
	c.Node = ws.Node

	// What this wake's pod used, from its start through the command.
	if s, err := r.Cluster.Sidecar(ctx, ns, name, sidecarScript); err == nil {
		c.Sidecar, err = parseSidecar(s)
		if err != nil {
			r.say("  cycle %d: sidecar usage: %v", n, err)
		}
	} else {
		r.say("  cycle %d: sidecar usage: %v", n, err)
	}
	if a, err := r.Gateway.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"sh", "-c", agentScript}, WaitFor: []string{"none"}}); err == nil {
		if c.Agent, err = parseAgent(a.Stdout); err != nil {
			r.say("  cycle %d: agent usage: %v", n, err)
		}
	} else {
		r.say("  cycle %d: agent usage: %v", n, err)
	}

	after, err := r.counters(ctx)
	if err != nil {
		return c, epoch, err
	}
	c.S3 = after.Since(before)
	mode := "no hint"
	if hinted {
		mode = "hint"
	}
	r.say("  cycle %d (%s): suspend %s, files %s, serving %s, deps %s (%s), agent waited %s, S3 %.0f put + %.0f get",
		n, mode, secs(c.SuspendSeconds), secs(c.FilesReadySeconds), secs(c.ServingSeconds), secs(c.DepsReadySeconds),
		c.DepsSource, secs(c.AgentWaitSeconds), c.S3.PutClass, c.S3.GetClass)
	return c, ws.Epoch, nil
}

// command runs the dependency command; failing is an error, as the
// numbers would mean nothing.
func (r *Runner) command(ctx context.Context, ns, name, script string) (*sbxapi.ExecResponse, error) {
	out, err := r.Gateway.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"sh", "-c", script}, TimeoutSeconds: 600})
	if err != nil {
		return nil, fmt.Errorf("running %q: %w", script, err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("running %q: exit %d: %s", script, out.ExitCode, tail(out.Stdout+out.Stderr, 400))
	}
	return out, nil
}

// wipe drops the cached base bundles of repo and every archive of its
// dependency profile, so nothing is reused from an earlier run.
func (r *Runner) wipe(ctx context.Context, repo Repo) error {
	for _, prefix := range []string{
		path.Dir(snapstore.BaseBundleKey(repo.URL, repo.Commit)) + "/",
		path.Dir(snapstore.DepsKey(repo.Profile, "x")) + "/",
	} {
		objs, err := r.Store.List(ctx, prefix)
		if err != nil {
			return fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, o := range objs {
			if err := r.Store.Delete(ctx, o.Key); err != nil {
				return fmt.Errorf("deleting %s: %w", o.Key, err)
			}
		}
		if len(objs) > 0 {
			r.say("  wiped %d cached objects under %s", len(objs), prefix)
		}
	}
	return nil
}

// archiveBytes is the size of the workspace's dependency archive.
func (r *Runner) archiveBytes(ctx context.Context, ns, name, profile string) (int64, error) {
	ws, err := r.Gateway.Get(ctx, ns, name)
	if err != nil {
		return 0, err
	}
	if ws.Deps == nil || ws.Deps.Key == "" {
		return 0, nil
	}
	if r.Store != nil {
		if o, err := r.Store.Head(ctx, snapstore.DepsKey(profile, ws.Deps.Key)); err == nil {
			return o.Size, nil
		}
	}
	return ws.Deps.Bytes, nil
}

func (r *Runner) counters(ctx context.Context) (Counters, error) {
	pages, err := r.Cluster.ControllerMetrics(ctx)
	if err != nil {
		return Counters{}, fmt.Errorf("reading controller metrics: %w", err)
	}
	return ParseMetrics(pages)
}

func (r *Runner) waitPhase(ctx context.Context, ns, name, phase string, timeout time.Duration) (*sbxapi.Workspace, error) {
	return r.waitFor(ctx, ns, name, timeout, phase, func(ws *sbxapi.Workspace) bool { return ws.Phase == phase })
}

// waitDeps waits until the dependency restore or install has settled.
func (r *Runner) waitDeps(ctx context.Context, ns, name string, timeout time.Duration) (*sbxapi.Workspace, error) {
	return r.waitFor(ctx, ns, name, timeout, "dependencies settled", func(ws *sbxapi.Workspace) bool {
		return ws.Phase == "Running" && ws.Deps != nil && !ws.Deps.Pending
	})
}

func (r *Runner) waitFor(ctx context.Context, ns, name string, timeout time.Duration, what string, ok func(*sbxapi.Workspace) bool) (*sbxapi.Workspace, error) {
	deadline := r.Now().Add(timeout)
	for {
		ws, err := r.Gateway.Get(ctx, ns, name)
		if err != nil {
			return nil, err
		}
		if ok(ws) {
			return ws, nil
		}
		if ws.Phase == "Failed" {
			return ws, fmt.Errorf("%s/%s failed: %s", ns, name, ws.Message)
		}
		if r.Now().After(deadline) {
			return ws, fmt.Errorf("%s/%s: no %s after %s (phase %s: %s)", ns, name, what, timeout, ws.Phase, ws.Message)
		}
		if err := r.Sleep(ctx, r.Poll); err != nil {
			return nil, err
		}
	}
}

// remove deletes a workspace and waits until it's gone, so the next
// repository has the cluster to itself.
func (r *Runner) remove(ctx context.Context, ns, name string) {
	if err := r.Gateway.Delete(ctx, ns, name); err != nil {
		r.say("  deleting %s/%s: %v", ns, name, err)
		return
	}
	deadline := r.Now().Add(5 * time.Minute)
	for r.Now().Before(deadline) {
		var se *sbxapi.StatusError
		if _, err := r.Gateway.Get(ctx, ns, name); errors.As(err, &se) && se.Status == 404 {
			return
		}
		if r.Sleep(ctx, time.Second) != nil {
			return
		}
	}
	r.say("  %s/%s is still being deleted", ns, name)
}

func (r *Runner) say(format string, args ...any) { fmt.Fprintf(r.Out, format+"\n", args...) }

// between is the seconds from a to b, or 0 if either is unknown.
func between(a, b *time.Time) float64 {
	if a == nil || b == nil {
		return 0
	}
	return b.Sub(*a).Seconds()
}

func secs(s float64) string { return fmt.Sprintf("%.1fs", s) }

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
