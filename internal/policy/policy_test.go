package policy_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func fixed(t *testing.T, params map[string]string) policy.Policy {
	t.Helper()
	p, err := policy.New("fixed", params)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFixedDecisions(t *testing.T) {
	p := fixed(t, map[string]string{"idleTimeout": "10m", "afterTurnEnded": "2m"})
	ws := policy.Workspace{Key: "ns/w", AwakeSince: t0}
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	cases := []struct {
		name      string
		s         policy.Signals
		now       time.Time
		suspend   bool
		reason    policy.Reason
		suspendAt time.Time
	}{
		{name: "just woke", now: at(time.Minute), reason: policy.ReasonActive, suspendAt: at(10 * time.Minute)},
		{name: "idle since waking", now: at(10 * time.Minute), suspend: true, reason: policy.ReasonIdle},
		{name: "recent activity", s: policy.Signals{LastActivity: at(8 * time.Minute)}, now: at(12 * time.Minute),
			reason: policy.ReasonActive, suspendAt: at(18 * time.Minute)},
		{name: "idle after activity", s: policy.Signals{LastActivity: at(time.Minute)}, now: at(11 * time.Minute),
			suspend: true, reason: policy.ReasonIdle},
		{name: "turn ended, waiting", s: policy.Signals{LastActivity: at(time.Minute), LastTurnEnded: at(time.Minute)},
			now: at(2 * time.Minute), reason: policy.ReasonActive, suspendAt: at(3 * time.Minute)},
		{name: "turn ended, quiet since", s: policy.Signals{LastActivity: at(time.Minute), LastTurnEnded: at(time.Minute)},
			now: at(3 * time.Minute), suspend: true, reason: policy.ReasonTurnEnded},
		{name: "activity after the turn ended", s: policy.Signals{LastTurnEnded: at(time.Minute), LastActivity: at(2 * time.Minute)},
			now: at(5 * time.Minute), reason: policy.ReasonActive, suspendAt: at(12 * time.Minute)},
		{name: "a new turn started", s: policy.Signals{LastTurnEnded: at(time.Minute), LastTurnStarted: at(2 * time.Minute)},
			now: at(5 * time.Minute), reason: policy.ReasonActive, suspendAt: at(12 * time.Minute)},
		{name: "running exec vetoes", now: at(time.Hour), reason: policy.ReasonCommandRunning,
			s: policy.Signals{Commands: []policy.Command{{Kind: policy.CommandExec, Command: "npm test"}}}},
		{name: "running service doesn't", now: at(time.Hour), suspend: true, reason: policy.ReasonIdle,
			s: policy.Signals{Commands: []policy.Command{{Kind: policy.CommandService, Command: "npm run dev"}}}},
		{name: "open app stream vetoes", now: at(time.Hour), reason: policy.ReasonStreamOpen,
			s: policy.Signals{Streams: map[policy.StreamKind]int{policy.StreamApp: 1}}},
		{name: "open editor doesn't", now: at(time.Hour), suspend: true, reason: policy.ReasonIdle,
			s: policy.Signals{Streams: map[policy.StreamKind]int{policy.StreamEditor: 2}}},
		{name: "held requests veto", now: at(time.Hour), reason: policy.ReasonRequestsHeld, s: policy.Signals{HeldRequests: 1}},
		{name: "unknown signals keep", now: at(time.Hour), reason: policy.ReasonSignalsUnknown, s: policy.Signals{Unknown: []string{"agent"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := p.Decide(ws, c.s, c.now)
			if d.Suspend != c.suspend || d.Reason != c.reason || !d.SuspendAt.Equal(c.suspendAt) {
				t.Fatalf("got suspend=%v reason=%s at=%v (%q); want suspend=%v reason=%s at=%v",
					d.Suspend, d.Reason, d.SuspendAt, d.Message, c.suspend, c.reason, c.suspendAt)
			}
			if !d.Suspend && d.RecheckAfter <= 0 {
				t.Errorf("a kept workspace must be rechecked, got %v", d.RecheckAfter)
			}
			if !d.SuspendAt.IsZero() && d.RecheckAfter != d.SuspendAt.Sub(c.now) {
				t.Errorf("recheck %v, want at the deadline (%v)", d.RecheckAfter, d.SuspendAt.Sub(c.now))
			}
		})
	}
}

func TestTurnEndedCanBeTurnedOff(t *testing.T) {
	p := fixed(t, map[string]string{"idleTimeout": "10m", "afterTurnEnded": "0"})
	s := policy.Signals{LastActivity: t0, LastTurnEnded: t0}
	if d := p.Decide(policy.Workspace{AwakeSince: t0}, s, t0.Add(5*time.Minute)); d.Suspend {
		t.Fatalf("suspended 5m after a turn ended with afterTurnEnded off: %+v", d)
	}
}

func TestBlockingKindsAreConfigurable(t *testing.T) {
	p := fixed(t, map[string]string{"blockingCommands": "service", "blockingStreams": "editor"})
	ws := policy.Workspace{AwakeSince: t0}
	now := t0.Add(time.Hour)
	if d := p.Decide(ws, policy.Signals{Commands: []policy.Command{{Kind: policy.CommandExec}}}, now); !d.Suspend {
		t.Errorf("exec blocked although only services block: %+v", d)
	}
	if d := p.Decide(ws, policy.Signals{Commands: []policy.Command{{Kind: policy.CommandService}}}, now); d.Suspend {
		t.Error("a service didn't block")
	}
	if d := p.Decide(ws, policy.Signals{Streams: map[policy.StreamKind]int{policy.StreamEditor: 1}}, now); d.Suspend {
		t.Error("an editor stream didn't block")
	}
}

func TestNeverKeeps(t *testing.T) {
	p, err := policy.New("never", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Decide(policy.Workspace{AwakeSince: t0}, policy.Signals{}, t0.Add(1000*time.Hour)); d.Suspend || d.RecheckAfter != 0 {
		t.Fatalf("never: %+v", d)
	}
}

func TestBadParamsAreRejected(t *testing.T) {
	for _, c := range []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"fixed", map[string]string{"idleTimout": "1m"}, `no parameter "idleTimout"`},
		{"fixed", map[string]string{"idleTimeout": "1s"}, "at least 5s"},
		{"fixed", map[string]string{"idleTimeout": "soon"}, "idleTimeout"},
		{"fixed", map[string]string{"afterTurnEnded": "-1m"}, "afterTurnEnded"},
		{"fixed", map[string]string{"blockingCommands": "exec,cron"}, `unknown kind "cron"`},
		{"fixed", map[string]string{"blockingStreams": "ws"}, `unknown kind "ws"`},
		{"never", map[string]string{"idleTimeout": "1m"}, "no parameter"},
		{"sometimes", nil, `unknown policy "sometimes" (have fixed, never)`},
	} {
		_, err := policy.New(c.name, c.params)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%s, %v) = %v, want an error containing %q", c.name, c.params, err, c.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	c, p, err := policy.ParseConfig([]byte("name: fixed\nparams:\n  idleTimeout: 30s\n"))
	if err != nil || p.Name() != "fixed" || c.Params["idleTimeout"] != "30s" {
		t.Fatalf("got %+v, %v, %v", c, p, err)
	}
	for _, bad := range []string{"", "name: fixed\nidleTimeout: 30s\n", "name: [", "name: fixed\nparams:\n  idleTimeout: 1s\n"} {
		if _, _, err := policy.ParseConfig([]byte(bad)); err == nil {
			t.Errorf("ParseConfig(%q) accepted", bad)
		}
	}
}

func TestSpecsListParamsWithDefaults(t *testing.T) {
	specs := policy.Specs()
	if len(specs) < 2 || specs[0].Name != "fixed" || specs[1].Name != "never" {
		t.Fatalf("specs = %v", specs)
	}
	for _, p := range specs[0].Params {
		if p.Default == "" || p.Help == "" {
			t.Errorf("fixed parameter %s lacks a default or help", p.Name)
		}
	}
}

func TestMergeAndFromProto(t *testing.T) {
	agent := policy.FromProto(&signalsv1.Signals{
		LastActivity:    timestamppb.New(t0),
		RunningCommands: []*signalsv1.Command{{Command: "make", Kind: signalsv1.CommandKind_COMMAND_KIND_TERMINAL}},
		OpenStreams:     []*signalsv1.Stream{{Kind: signalsv1.StreamKind_STREAM_KIND_EXEC, Count: 1}},
	})
	gw1 := policy.FromProto(&signalsv1.Signals{LastActivity: timestamppb.New(t0.Add(time.Minute)), HeldRequests: 2,
		LastTurnEnded: timestamppb.New(t0.Add(2 * time.Minute))})
	gw2 := policy.FromProto(&signalsv1.Signals{HeldRequests: 1, LastTurnStarted: timestamppb.New(t0.Add(3 * time.Minute)),
		OpenStreams: []*signalsv1.Stream{{Kind: signalsv1.StreamKind_STREAM_KIND_EXEC, Count: 2}}})
	var s policy.Signals
	for _, o := range []policy.Signals{agent, gw1, gw2} {
		s.Merge(o)
	}
	if !s.LastActivity.Equal(t0.Add(time.Minute)) || !s.LastTurnEnded.Equal(t0.Add(2*time.Minute)) ||
		!s.LastTurnStarted.Equal(t0.Add(3*time.Minute)) || s.HeldRequests != 3 ||
		s.Streams[policy.StreamExec] != 3 || len(s.Commands) != 1 || s.Commands[0].Kind != policy.CommandTerminal {
		t.Fatalf("merged = %+v", s)
	}
}
