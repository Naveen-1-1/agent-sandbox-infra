package costsim

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// A tiny trace language, in seconds:
//
//	H@t   a person's prompt     X@t    model text
//	u@t   a non-human message   B[a,b] a Bash call (B* needs dependencies)
//	Q[a,b] AskUserQuestion      T[a,b] a Task call   W[a,b] a web search
func us(sec float64) int64 { return int64(sec * 1e6) }

func human(t float64) Event { return Event{T: us(t), User: true, Human: true} }
func text(t float64) Event  { return Event{T: us(t)} }
func meta(t float64) Event  { return Event{T: us(t), User: true} }

func call(name string, s, e float64) Tool {
	end := us(e)
	return Tool{Name: name, S: us(s), E: &end}
}

func session(key, root string, evs []Event, tools []Tool) Session {
	return Session{Key: key, Root: root, Provider: strings.Split(key, ":")[0], Events: evs, Tools: tools}
}

func timeline(evs []Event, tools []Tool, children ...Session) Timeline {
	g := Group{Key: "claude:p", Provider: "claude", Members: append([]Session{session("claude:p", "claude:p", evs, tools)}, children...)}
	return Build(g, BuildOptions{Deps: DepsAuto})
}

// traceA: a prompt, a call, text; a reply 90s later, a call, text.
func traceA() Timeline {
	return timeline([]Event{human(0), text(10), human(100), text(120)},
		[]Tool{call("Bash", 5, 10), call("Bash", 115, 120)})
}

func classes(gaps []Gap) map[Class]float64 {
	out := map[Class]float64{}
	for _, g := range gaps {
		out[g.Class] += float64(g.E-g.S) / 1e6
	}
	return out
}

func TestGapClassification(t *testing.T) {
	for _, c := range []struct {
		name string
		tl   Timeline
		want map[Class]float64
	}{
		{"two turns", traceA(), map[Class]float64{ClassToolRunning: 10, ClassAIThinking: 20, ClassHumanWait: 90, ClassTail: 60}},
		{"parallel calls count once", timeline([]Event{human(0), text(45)}, []Tool{call("Bash", 10, 30), call("Bash", 20, 40)}),
			map[Class]float64{ClassToolRunning: 30, ClassAIThinking: 15, ClassTail: 60}},
		{"a running call beats waiting for a person", timeline([]Event{human(0), text(5), human(60), text(75)}, []Tool{call("Bash", 10, 70)}),
			map[Class]float64{ClassToolRunning: 60, ClassAIThinking: 15, ClassTail: 60}},
		{"a question is human time", timeline([]Event{human(0), text(60)}, []Tool{call("Bash", 1, 2), call("AskUserQuestion", 5, 50), call("Bash", 55, 56)}),
			map[Class]float64{ClassToolRunning: 2, ClassAIThinking: 13, ClassHumanWait: 45, ClassTail: 60}},
		{"subagent calls are sandbox time; its prompt is not a turn",
			timeline([]Event{human(0), text(105)}, []Tool{call("Task", 10, 100)},
				session("claude:p/subagents/a", "claude:p", []Event{human(11), text(95)}, []Tool{call("Bash", 20, 30), call("Bash", 50, 60)})),
			map[Class]float64{ClassToolRunning: 20, ClassAIThinking: 85, ClassTail: 60}},
		{"a web search is the model's time", timeline([]Event{human(0), text(25)}, []Tool{call("WebSearch", 10, 20)}),
			map[Class]float64{ClassAIThinking: 25, ClassTail: 60}},
		{"records before the first prompt", timeline([]Event{meta(0), human(2), text(5)}, []Tool{call("Bash", 3, 4)}),
			map[Class]float64{ClassHumanWait: 2, ClassAIThinking: 2, ClassToolRunning: 1, ClassTail: 60}},
		{"no prompt at all", timeline([]Event{text(0), text(3)}, []Tool{call("Bash", 1, 2)}),
			map[Class]float64{ClassAIThinking: 2, ClassToolRunning: 1, ClassTail: 60}},
	} {
		got := classes(Gaps(c.tl, us(60)))
		for _, cl := range Classes {
			if math.Abs(got[cl]-c.want[cl]) > 1e-9 {
				t.Errorf("%s: %s = %v, want %v (all: %v)", c.name, cl, got[cl], c.want[cl], got)
			}
		}
	}
}

func TestBuildFindsTurnsAndFirstCalls(t *testing.T) {
	tl := traceA()
	if len(tl.Turns) != 2 || tl.Turns[0].End != us(10) || tl.Turns[1].End != us(120) {
		t.Errorf("turns = %+v", tl.Turns)
	}
	if len(tl.Inputs) != 2 || tl.Inputs[0].First != us(5) || tl.Inputs[1].First != us(115) {
		t.Errorf("inputs = %+v", tl.Inputs)
	}
	open := timeline([]Event{human(0)}, []Tool{{Name: "Bash", S: us(10)}, call("Bash", 20, 15), {ID: "x", Name: "Read", S: us(1)}, {ID: "x", Name: "Read", S: us(1)}, call("mcp__x", 1, 2)})
	if q := open.Quality; q.OpenTools != 2 || q.NegativeTools != 1 || q.DuplicateTools != 1 || q.OtherTools["mcp__x"] != 1 {
		t.Errorf("quality = %+v", q)
	}
	dep := Build(Group{Members: []Session{session("claude:p", "claude:p", []Event{human(0)},
		[]Tool{{Name: "Bash", S: 1, E: ptr(int64(2)), Exe: []string{"/usr/bin/npm"}}, {Name: "Bash", S: 3, E: ptr(int64(4)), Exe: []string{"ls"}}})}},
		BuildOptions{Deps: DepsAuto})
	if !dep.Calls[0].Deps || dep.Calls[1].Deps {
		t.Errorf("deps = %+v; want only the npm call", dep.Calls)
	}
}

func ptr[T any](v T) *T { return &v }

func TestGroupSessions(t *testing.T) {
	all := []Session{
		session("claude:a/subagents/x", "claude:a", []Event{text(5)}, nil),
		session("claude:a", "claude:a", []Event{human(0)}, nil),
		session("claude:a/subagents/x/subagents/y", "claude:a", []Event{text(6)}, nil),
		session("claude:b/subagents/z", "claude:b", []Event{text(1)}, nil), // its parent isn't in the trace
		session("codex:c", "codex:c", []Event{human(0)}, nil),
		session("codex:d", "codex:d", nil, nil), // nothing timestamped
	}
	groups, st := GroupSessions(all)
	if st.Raw["total"] != 6 || st.Subagents["claude"] != 3 || st.Roots["claude"] != 2 || st.Roots["codex"] != 2 || st.Roots["total"] != 4 {
		t.Errorf("stats = %+v", st)
	}
	if st.Orphans != 1 || st.Empty != 1 || len(groups) != 3 {
		t.Errorf("orphans %d, empty %d, groups %d", st.Orphans, st.Empty, len(groups))
	}
	if g := groups[0]; g.Key != "claude:a" || len(g.Members) != 3 || g.Members[0].Key != "claude:a" {
		t.Errorf("first group = %+v; want the parent first, then its subagents", g)
	}
}

// bench is a platform that wakes in 10s (20s with dependencies) and
// suspends in 5s.
var bench = Platform{WakeServe: 10 * time.Second, WakeDeps: 20 * time.Second, Suspend: 5 * time.Second}

func run(t *testing.T, tl Timeline, name string, params map[string]string, hints bool, tail float64) Result {
	t.Helper()
	pol, err := policy.New(name, params)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Simulate(tl, pol, hints, bench, time.Duration(tail*float64(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func fixed(idle string) map[string]string {
	return map[string]string{"idleTimeout": idle, "afterTurnEnded": "0"}
}

func secs(d time.Duration) float64 { return d.Seconds() }

func TestSimulate(t *testing.T) {
	type want struct {
		billed, asleep           float64
		suspends, aborts, wakes  int
		waits                    []float64
		userWakes, hidden, extra int // extra: unused hints
	}
	check := func(name string, r Result, w want) {
		t.Helper()
		var waits []float64
		for _, h := range r.Holds {
			waits = append(waits, secs(h.Wait))
		}
		if secs(r.AwakeBilled()) != w.billed || secs(r.Asleep) != w.asleep || r.Suspends != w.suspends ||
			r.Aborts != w.aborts || r.Wakes() != w.wakes || len(waits) != len(w.waits) ||
			r.UserMessageWakes != w.userWakes || r.Hidden != w.hidden || r.UnusedHints != w.extra {
			t.Errorf("%s:\n got billed %v asleep %v suspends %d aborts %d wakes %d waits %v user %d/%d unused %d\nwant %+v",
				name, secs(r.AwakeBilled()), secs(r.Asleep), r.Suspends, r.Aborts, r.Wakes(), waits, r.Hidden, r.UserMessageWakes, r.UnusedHints, w)
			return
		}
		for i := range waits {
			if waits[i] != w.waits[i] {
				t.Errorf("%s: waits %v, want %v", name, waits, w.waits)
			}
		}
	}

	check("never sleeps", run(t, traceA(), "never", nil, false, 60), want{billed: 180})
	check("a call longer than the idle timeout keeps it up",
		run(t, timeline([]Event{human(0), text(100)}, []Tool{call("Bash", 10, 100)}), "fixed", fixed("30s"), true, 60),
		want{billed: 135, asleep: 25, suspends: 1})
	check("the hint hides the wake", run(t, traceA(), "fixed", fixed("20s"), true, 60),
		want{billed: 80, asleep: 100, suspends: 2, wakes: 1, userWakes: 1, hidden: 1})
	check("a call before the wake finishes waits for the rest",
		run(t, timeline([]Event{human(0), text(10), human(100), text(109)}, []Tool{call("Bash", 5, 10), call("Bash", 104, 109)}), "fixed", fixed("20s"), true, 60),
		want{billed: 75, asleep: 100, suspends: 2, wakes: 1, waits: []float64{6}, userWakes: 1})
	check("parallel calls are held once",
		run(t, timeline([]Event{human(0), text(10), human(100), text(109)}, []Tool{call("Bash", 5, 10), call("Bash", 104, 109), call("Read", 104, 107)}), "fixed", fixed("20s"), true, 60),
		want{billed: 75, asleep: 100, suspends: 2, wakes: 1, waits: []float64{6}, userWakes: 1})
	check("without a hint the agent waits for the whole wake", run(t, traceA(), "fixed", fixed("20s"), false, 60),
		want{billed: 75, asleep: 115, suspends: 2, wakes: 1, waits: []float64{10}, userWakes: 1})
	check("a call during a suspend cuts it short",
		run(t, timeline([]Event{human(0), text(10), text(33)}, []Tool{call("Bash", 5, 10), call("Bash", 32, 33)}), "fixed", fixed("20s"), true, 60),
		want{billed: 58, asleep: 35, suspends: 2, aborts: 1})
	check("a call after the suspend wakes it mid-turn",
		run(t, timeline([]Event{human(0), text(10), text(36)}, []Tool{call("Bash", 5, 10), call("Bash", 35, 36)}), "fixed", fixed("20s"), true, 60),
		want{billed: 71, asleep: 35, suspends: 2, wakes: 1, waits: []float64{10}})
	check("a hint aborts a suspend for the next call",
		run(t, timeline([]Event{human(0), text(10), human(32), text(41)}, []Tool{call("Bash", 5, 10), call("Bash", 40, 41)}), "fixed", fixed("20s"), true, 60),
		want{billed: 66, asleep: 35, suspends: 2, aborts: 1, userWakes: 1, hidden: 1})
	check("a hint no call follows is a wasted wake",
		run(t, timeline([]Event{human(0), text(10), human(100), text(105)}, []Tool{call("Bash", 5, 10)}), "fixed", fixed("20s"), true, 60),
		want{billed: 70, asleep: 95, suspends: 2, wakes: 1, extra: 1})
	check("a deadline at a call's start loses to the call",
		run(t, timeline([]Event{human(0), text(31)}, []Tool{call("Bash", 5, 10), call("Bash", 30, 31)}), "fixed", fixed("20s"), true, 5),
		want{billed: 36})
	check("deleted before the idle timeout", run(t, timeline([]Event{human(0), text(10)}, []Tool{call("Bash", 5, 10)}), "fixed", fixed("20s"), true, 5),
		want{billed: 15})

	// A call that needs dependencies waits for them, after a hinted wake.
	deps := timeline([]Event{human(0), text(10), human(100), text(120)}, []Tool{call("Bash", 5, 10),
		{Name: "Bash", S: us(115), E: ptr(us(120)), Exe: []string{"npm"}}})
	check("dependencies come after the files", run(t, deps, "fixed", fixed("20s"), true, 60),
		want{billed: 85, asleep: 100, suspends: 2, wakes: 1, waits: []float64{5}, userWakes: 1})

	// The hint counts as activity, as the gateway records a turn started.
	tl := timeline([]Event{human(0), text(2), human(50), text(55)}, []Tool{call("Bash", 1, 2)})
	if h, u := run(t, tl, "fixed", fixed("60s"), true, 120), run(t, tl, "fixed", fixed("60s"), false, 120); secs(h.AwakeBilled()) != 115 || secs(u.AwakeBilled()) != 67 {
		t.Errorf("suspend after a hint at 50: awake %v; without: %v (want 115 and 67)", secs(h.AwakeBilled()), secs(u.AwakeBilled()))
	}

	// Ending the turn lets the after-turn timer suspend it sooner: 2m
	// after the first turn instead of 15m. The session is gone 60s after
	// the second, before its timer.
	turn := timeline([]Event{human(0), text(10), human(1000), text(1011)}, []Tool{call("Bash", 5, 10), call("Bash", 1010, 1011)})
	adaptive := run(t, turn, "fixed", map[string]string{"idleTimeout": "15m", "afterTurnEnded": "2m"}, true, 60)
	if adaptive.Suspends != 1 || secs(adaptive.Awake) != 130+61 || adaptive.Hidden != 1 || len(adaptive.Holds) != 0 {
		t.Errorf("adaptive: %+v", adaptive)
	}
	if idle := run(t, turn, "fixed", fixed("15m"), true, 60); idle.Suspends != 1 || secs(idle.AwakeBilled()) != 910+15+61 {
		t.Errorf("15m without the after-turn timer: %+v", idle)
	}

	if a, b := run(t, traceA(), "fixed", fixed("20s"), true, 60), run(t, traceA(), "fixed", fixed("20s"), true, 60); a.AwakeBilled() != b.AwakeBilled() || len(a.Holds) != len(b.Holds) {
		t.Error("the same input gave different results")
	}
}

func TestPrices(t *testing.T) {
	p := Prices{AwakeUsdPerHour: 3.6, ObjectGiBMonth: 7.3, PutPer1k: 0.005, GetPer1k: 0.0004}
	r := run(t, traceA(), "fixed", fixed("20s"), true, 60)
	c := p.Of(r, Usage{StoredBytes: 1 << 30, PutRequestsPerCycle: 10, GetRequestsPerCycle: 20})
	// 80s awake at $0.001/s; 100s asleep at $0.01/h; 2 suspends of 10 PUTs; 1 wake of 20 GETs.
	if math.Abs(c.TotalUsd-0.080385778) > 1e-9 || math.Abs(c.RequestsUsd-0.000108) > 1e-12 {
		t.Errorf("cost = %+v", c)
	}
	hour := Result{Awake: time.Hour, Asleep: 730 * time.Hour}
	c = Reference.Of(hour, Usage{StoredBytes: 1 << 30})
	if math.Abs(c.AwakeUsd-0.0816) > 1e-12 || math.Abs(c.StorageUsd-0.023) > 1e-12 {
		t.Errorf("reference prices = %+v", c)
	}
	if c := Reference.Of(Result{Suspends: 1000, HintWakes: 1000}, Usage{PutRequestsPerCycle: 1, GetRequestsPerCycle: 1}); math.Abs(c.RequestsUsd-(0.005+0.0004)) > 1e-12 {
		t.Errorf("1000 cycles of one request each: %+v", c)
	}
}
