// Package demo plays an agent session against a running platform, using
// only the gateway API, the way an agent harness would. The workspace goes
// to sleep when the agent ends its turn; when the user replies, a wake-hint
// starts the wake while the LLM is still thinking. The same is then done
// without a hint, so the visible and hidden wakes can be compared.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

// Options configure a run.
type Options struct {
	RepoURL   string
	Image     string // empty: the platform's choice
	Namespace string // empty: the gateway's default
	Name      string // empty: generated
	// Thinking is how long the simulated LLM thinks after the user's
	// message before its first tool call.
	Thinking time.Duration
	// AfterTurn is how soon the workspace sleeps once the agent ends its turn.
	AfterTurn time.Duration
	// Keep leaves the workspace in place afterwards.
	Keep bool
	Out  io.Writer
}

// Result is what a run measured.
type Result struct {
	Workspace   string        `json:"workspace"`
	Provisioned time.Duration `json:"provisioned"`
	// Suspended is how long after each turn ended the workspace was asleep.
	Suspended     []time.Duration `json:"suspended"`
	SnapshotBytes int64           `json:"snapshotBytes"`
	Hinted        Wake            `json:"hinted"`
	Unhinted      Wake            `json:"unhinted"`
}

// Wake is one wake as the agent saw it. Stage times are from the wake
// request (the hint, or the agent's own command).
type Wake struct {
	Node       string        `json:"node"`
	PodAt      time.Duration `json:"podAt"`
	FilesAt    time.Duration `json:"filesAt"`
	ServingAt  time.Duration `json:"servingAt"`
	DepsAt     time.Duration `json:"depsAt"`
	DepsSource string        `json:"depsSource"`
	CommandAt  time.Duration `json:"commandAt"` // when the agent's first command (npm test) was sent
	Waited     time.Duration `json:"waited"`    // how long that command was held for the wake
	// DepsWaited is how long it then waited for node_modules.
	DepsWaited  time.Duration `json:"depsWaited"`
	StateIntact bool          `json:"stateIntact"`
}

// The agent's work in its first turn (on the hello-deps fixture), the
// command that fingerprints it, and the agent's first tool call after the
// user replies, which needs the dependencies.
const (
	turnScript = `git checkout -q -b agent/feature &&
printf 'module.exports = () => "ready";\n' > feature.js &&
git add feature.js &&
printf '// TODO: use feature.js\n' >> test.js`
	planFile     = "PLAN.md"
	planContent  = "# Plan\n\n- add feature.js\n- use it from test.js\n"
	fingerprint  = `git status --short --branch && sha256sum feature.js test.js PLAN.md`
	firstCommand = "npm test --silent"
)

// Run plays the session. On success the workspace is deleted unless
// o.Keep; on failure it is kept for inspection.
func Run(ctx context.Context, admin *sbxapi.Client, o Options) (*Result, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Thinking <= 0 {
		o.Thinking = 10 * time.Second
	}
	if o.AfterTurn <= 0 {
		o.AfterTurn = 5 * time.Second
	}
	if o.Name == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		o.Name = "demo-" + hex.EncodeToString(b)
	}
	d := &run{o: o, out: o.Out, res: &Result{}, admin: admin}
	err := d.play(ctx)
	if d.ws != nil {
		switch {
		case err != nil:
			d.say("\nkept %s for inspection", d.res.Workspace)
		case !o.Keep:
			if derr := admin.Delete(context.WithoutCancel(ctx), d.ns, d.name); derr != nil {
				d.say("deleting %s: %v", d.res.Workspace, derr)
			}
		}
	}
	return d.res, err
}

type run struct {
	o        Options
	out      io.Writer
	res      *Result
	admin    *sbxapi.Client // creates and deletes
	c        *sbxapi.Client // everything else, with the workspace's own token as a harness would
	ws       *sbxapi.Workspace
	ns, name string
	want     string // the fingerprint after the agent's turn
	step     int
}

func (d *run) play(ctx context.Context) error {
	d.say("sbx demo: an agent session that sleeps between turns and wakes before it's needed")

	d.header("Create a workspace from %s", d.o.RepoURL)
	start := time.Now()
	resp, err := d.admin.Create(ctx, sbxapi.CreateRequest{
		Namespace: d.o.Namespace, Name: d.o.Name, RepoURL: d.o.RepoURL, Image: d.o.Image,
		Policy: &sbxapi.PolicyRef{Name: "fixed", Params: map[string]string{
			"idleTimeout": "10m", "afterTurnEnded": d.o.AfterTurn.String(),
		}},
	})
	if err != nil {
		return fmt.Errorf("creating the workspace: %w", err)
	}
	d.ns, d.name = resp.Workspace.Namespace, resp.Workspace.Name
	d.res.Workspace = d.ns + "/" + d.name
	d.c = &sbxapi.Client{BaseURL: d.admin.BaseURL, Token: resp.Token, HTTP: d.admin.HTTP}
	if d.ws, err = d.waitPhase(ctx, "Running", 10*time.Minute); err != nil {
		return err
	}
	d.res.Provisioned = time.Since(start)
	d.say("   %s is serving on %s after %s (the first start pulls the image)", d.res.Workspace, d.ws.Node, secs(d.res.Provisioned))
	if d.ws, err = d.waitDeps(ctx); err != nil {
		return err
	}
	d.say("   dependencies: %s", depsSummary(d.ws))

	d.header("Agent turn: write a plan, make a branch, stage one change, leave another unstaged")
	if err := d.c.WriteFile(ctx, d.ns, d.name, planFile, []byte(planContent)); err != nil {
		return fmt.Errorf("writing %s: %w", planFile, err)
	}
	if _, err := d.exec(ctx, turnScript); err != nil {
		return err
	}
	out, err := d.exec(ctx, fingerprint)
	if err != nil {
		return err
	}
	d.want = out.Stdout
	d.say("   $ git status --short --branch")
	for _, l := range statusLines(out.Stdout) {
		d.say("   %s", l)
	}

	if err := d.endTurn(ctx); err != nil {
		return err
	}
	if d.ws.LastSnapshot != nil {
		d.res.SnapshotBytes = d.ws.LastSnapshot.Bytes
	}
	d.say("   %s saved to S3; the pod and disk are gone, so it costs only storage now", size(d.res.SnapshotBytes))

	d.header("The user replies: the harness sends a wake-hint, then the LLM thinks for %s", secs(d.o.Thinking))
	hintAt := time.Now()
	if err := d.c.WakeHint(ctx, d.ns, d.name); err != nil {
		return fmt.Errorf("sending the wake-hint: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d.o.Thinking):
	}
	if d.res.Hinted, err = d.reply(ctx, hintAt); err != nil {
		return err
	}
	d.report(d.res.Hinted, "wake-hint", "the wake was hidden behind the thinking")

	if err := d.endTurn(ctx); err != nil {
		return err
	}
	d.header("The user replies again, this time without a wake-hint")
	if d.res.Unhinted, err = d.reply(ctx, time.Now()); err != nil {
		return err
	}
	d.report(d.res.Unhinted, "request", "the agent waited for the whole wake")

	d.summary()
	if !d.res.Hinted.StateIntact || !d.res.Unhinted.StateIntact {
		return errors.New("the workspace's git state changed across a sleep")
	}
	return nil
}

// endTurn ends the agent's turn and waits for the policy to suspend.
func (d *run) endTurn(ctx context.Context) error {
	d.header("The agent ends its turn; the policy suspends the workspace %s later", secs(d.o.AfterTurn))
	ended := time.Now()
	if err := d.c.TurnEnded(ctx, d.ns, d.name); err != nil {
		return fmt.Errorf("ending the turn: %w", err)
	}
	ws, err := d.waitPhase(ctx, "Suspended", 3*time.Minute)
	if err != nil {
		return err
	}
	d.ws = ws
	took := time.Since(ended)
	d.res.Suspended = append(d.res.Suspended, took)
	d.say("   asleep %s after the turn ended (%s)", secs(took), reason(ws))
	return nil
}

// reply sends the agent's first tool call after the user's reply (npm
// test, which needs node_modules) and measures its wake; requested is when
// the wake was asked for. It then checks the git state survived.
func (d *run) reply(ctx context.Context, requested time.Time) (Wake, error) {
	sent := time.Now()
	out, err := d.exec(ctx, firstCommand)
	if err != nil {
		return Wake{}, err
	}
	w := Wake{
		CommandAt: sent.Sub(requested), Waited: millisDuration(out.WaitedMillis),
		DepsWaited: millisDuration(out.DepsWaitedMillis),
	}
	fp, err := d.exec(ctx, fingerprint)
	if err != nil {
		return Wake{}, err
	}
	w.StateIntact = fp.Stdout == d.want
	ws, err := d.c.Get(ctx, d.ns, d.name)
	if err != nil {
		return Wake{}, err
	}
	d.ws, w.Node = ws, ws.Node
	if ws.Deps != nil {
		w.DepsSource = ws.Deps.Source
	}
	if tl := ws.Timeline; tl != nil && tl.RequestedAt != nil {
		at := func(t *time.Time) time.Duration {
			if t == nil {
				return 0
			}
			return t.Sub(*tl.RequestedAt)
		}
		w.PodAt, w.FilesAt, w.ServingAt, w.DepsAt = at(tl.PodScheduledAt), at(tl.FilesReadyAt), at(tl.ReadyAt), at(tl.DepsReadyAt)
	}
	return w, nil
}

// waitDeps waits for the dependency restore to settle.
func (d *run) waitDeps(ctx context.Context) (*sbxapi.Workspace, error) {
	for {
		ws, err := d.c.Get(ctx, d.ns, d.name)
		if err != nil {
			return nil, err
		}
		if ws.Deps == nil || !ws.Deps.Pending {
			return ws, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func depsSummary(ws *sbxapi.Workspace) string {
	switch {
	case ws.Deps == nil:
		return "none reported"
	case !ws.Deps.Ready:
		return "failed: " + ws.Deps.Message
	}
	return ws.Deps.Message
}

func millisDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

func (d *run) report(w Wake, first, verdict string) {
	if waited := w.Waited + w.DepsWaited; waited > 0 {
		verdict = fmt.Sprintf("%s: it waited %s", verdict, secs(waited))
	}
	d.say("   npm test waited %s for the wake and %s for node_modules; %s", millis(w.Waited), millis(w.DepsWaited), verdict)
	deps := "node_modules restored from the cache"
	if w.DepsSource == "Install" {
		deps = "node_modules installed (a cache miss)"
	}
	rows := []struct {
		label string
		at    time.Duration
		note  string
	}{
		{first, 0, ""},
		{"pod", w.PodAt, "scheduled on " + w.Node},
		{"files", w.FilesAt, "restored from S3"},
		{"serving", w.ServingAt, "commands that don't need dependencies run from here"},
		{"deps", w.DepsAt, deps},
		{"npm test", w.CommandAt + w.Waited + w.DepsWaited, "ran"},
	}
	scale := time.Duration(0)
	for _, r := range rows {
		scale = max(scale, r.at)
	}
	for _, r := range rows {
		d.say("     %-9s %6s  %-40s %s", r.label, secs(r.at), bar(r.at, scale, 40), r.note)
	}
	if w.StateIntact {
		d.say("   git state after the wake is identical: branch, staged, unstaged and untracked changes")
	} else {
		d.say("   git state after the wake DIFFERS from before the sleep")
	}
}

func (d *run) summary() {
	h, u := d.res.Hinted, d.res.Unhinted
	d.say("\nSummary              with hint   without")
	d.say("  agent waited       %9s  %8s", secs(h.Waited+h.DepsWaited), secs(u.Waited+u.DepsWaited))
	d.say("  files restored     %9s  %8s", secs(h.FilesAt), secs(u.FilesAt))
	d.say("  serving            %9s  %8s", secs(h.ServingAt), secs(u.ServingAt))
	d.say("  node_modules ready %9s  %8s", secs(h.DepsAt), secs(u.DepsAt))
	d.say("  (stage times are from the wake request; the hinted wake overlapped %s of thinking)", secs(d.o.Thinking))
}

func (d *run) exec(ctx context.Context, script string) (*sbxapi.ExecResponse, error) {
	out, err := d.c.Exec(ctx, d.ns, d.name, sbxapi.ExecRequest{Command: []string{"sh", "-c", script}})
	if err != nil {
		return nil, fmt.Errorf("running %q: %w", firstLine(script), err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("running %q: exit %d: %s", firstLine(script), out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	return out, nil
}

func (d *run) waitPhase(ctx context.Context, phase string, timeout time.Duration) (*sbxapi.Workspace, error) {
	deadline := time.Now().Add(timeout)
	for {
		ws, err := d.c.Get(ctx, d.ns, d.name)
		if err != nil {
			return nil, err
		}
		if ws.Phase == phase {
			return ws, nil
		}
		if ws.Phase == "Failed" || time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is %s (%s), want %s", d.res.Workspace, ws.Phase, ws.Message, phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *run) header(format string, args ...any) {
	d.step++
	d.say("\n%d. %s", d.step, fmt.Sprintf(format, args...))
}

func (d *run) say(format string, args ...any) { fmt.Fprintf(d.out, format+"\n", args...) }

func reason(ws *sbxapi.Workspace) string {
	if ws.Sleep == nil {
		return "no verdict recorded"
	}
	return ws.Sleep.Reason + ": " + ws.Sleep.Message
}

// statusLines drops the checksums from a fingerprint.
func statusLines(fp string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(fp), "\n") {
		if f := strings.Fields(l); len(f) == 2 && len(f[0]) == 64 {
			continue
		}
		out = append(out, l)
	}
	return out
}

func bar(at, scale time.Duration, width int) string {
	if scale <= 0 {
		return ""
	}
	return strings.Repeat("█", max(1, int(float64(width)*float64(at)/float64(scale)+0.5)))
}

func secs(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

func millis(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
