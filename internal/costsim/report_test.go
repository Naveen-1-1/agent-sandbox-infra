package costsim

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

var update = flag.Bool("update", false, "rewrite the golden report")

// reportSessions: a Claude session with a subagent and a reply after
// 20 minutes, and a Codex session that never returns to the sandbox.
func reportSessions() []Session {
	return []Session{
		session("claude:a", "claude:a", []Event{human(0), text(30), human(1230), text(1300)},
			[]Tool{call("Bash", 5, 25), call("Agent", 26, 29), call("Edit", 1240, 1241)}),
		session("claude:a/subagents/x", "claude:a", []Event{human(26), text(29)}, []Tool{call("Read", 27, 28)}),
		session("codex:b", "codex:b", []Event{human(0), text(10), human(600), text(620)}, []Tool{call("exec_command", 2, 9)}),
	}
}

func reportOptions() Options {
	return Options{
		Tail: 30 * time.Minute, Build: BuildOptions{Deps: DepsAuto}, Platform: bench, Prices: Reference,
		Usage:             Usage{StoredBytes: 50e6, PutRequestsPerCycle: 2, GetRequestsPerCycle: 8},
		SandboxUsdPerHour: 0.0408, ExpectedRoots: 2,
	}
}

func TestRunReport(t *testing.T) {
	groups, st := GroupSessions(reportSessions())
	r, err := Run(groups, st, Configs("15m", "2m"), reportOptions())
	if err != nil {
		t.Fatal(err)
	}
	if r.Sessions.Simulated != 2 || r.Sessions.Roots["total"] != 2 || r.Sessions.Subagents["claude"] != 1 {
		t.Fatalf("sessions = %+v", r.Sessions)
	}
	byName := map[string]ConfigResult{}
	for _, c := range r.Configs {
		byName[c.Name] = c
	}
	never, base, two := byName["never"], byName["fixed-15m-nohint"], byName["fixed-2m"]
	// Never sleeping: both sandboxes are up for their whole session and tail.
	if want := (1300.0 + 620.0 + 2*1800) / 3600; math.Abs(never.Hours.Awake-want) > 1e-9 {
		t.Errorf("never awake %v h, want %v", never.Hours.Awake, want)
	}
	// The baseline sleeps through the 20-minute reply and wakes on the
	// Edit; the 2-minute policy also sleeps through Codex's 10 minutes.
	if base.Suspends != 3 || base.RequestWakes != 1 || base.Waits.Visible != 1 {
		t.Errorf("baseline: %d suspends, %d request wakes, %d waits", base.Suspends, base.RequestWakes, base.Waits.Visible)
	}
	if two.HintWakes != 2 || two.UnusedHints != 1 || two.UserMessageWakes.Count != 1 || two.UserMessageWakes.Hidden != 1 {
		t.Errorf("fixed 2m: %+v", two)
	}
	if *two.SavedVsBase <= 0 || *base.SavedVsNever <= 0 || *two.Waits.ChangeVsBaseline != -1 {
		t.Errorf("savings: 2m vs base %v, base vs never %v, waits %v", *two.SavedVsBase, *base.SavedVsNever, *two.Waits.ChangeVsBaseline)
	}
	if r.AtScale.Scale != 0.5 {
		t.Errorf("scale = %v, want 0.0408/0.0816", r.AtScale.Scale)
	}
	for _, row := range r.AtScale.Rows {
		if math.Abs(row.PerYear["1M"]-1e6*row.SavedPerSessionRe*0.5) > 1e-6 {
			t.Errorf("%s: %v a year at 1M", row.Config, row.PerYear["1M"])
		}
	}
	// The subagent's Read runs inside the parent's Agent call: sandbox time.
	if g := r.Gaps[ClassToolRunning]; math.Abs(g.Hours*3600-(20+1+7+1)) > 1e-6 {
		t.Errorf("tool running %v s", g.Hours*3600)
	}

	var md bytes.Buffer
	r.Markdown(&md)
	golden := filepath.Join("testdata", "report.md")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, md.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got := md.String(); got != string(want) {
		t.Errorf("the report changed; check it and run go test ./internal/costsim -update\n%s", diffLines(string(want), got))
	}
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at line %d:\n- %s\n+ %s", i+1, x, y)
		}
	}
	return ""
}

func TestConfigsBuild(t *testing.T) {
	for _, c := range Configs("15m", "2m") {
		if _, err := policy.New(c.Policy, c.Params); err != nil {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
	if _, err := Run(nil, GroupStats{}, []Config{{Name: "bad", Policy: "fixed", Params: map[string]string{"idleTimeout": "1s"}}}, reportOptions()); err == nil {
		t.Error("an invalid config ran")
	}
}

func TestWaitDist(t *testing.T) {
	d := waitDist([]float64{0.5, 1.5, 6, 6, 12, 90})
	want := []int{1, 1, 0, 2, 1, 0, 0, 1}
	for i, b := range d.Buckets {
		if b.N != want[i] {
			t.Errorf("bucket ≤%v: %d, want %d", b.LE, b.N, want[i])
		}
	}
	if d.P50 != 6 || d.Max != 90 {
		t.Errorf("p50 %v max %v", d.P50, d.Max)
	}
}
