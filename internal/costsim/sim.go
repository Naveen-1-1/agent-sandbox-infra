package costsim

import (
	"fmt"
	"sort"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// Platform is how long the platform takes, as the bench measured it.
type Platform struct {
	WakeServe time.Duration // wake request until the workspace serves
	WakeDeps  time.Duration // wake request until the dependencies are back
	Suspend   time.Duration // suspend start until the pod is gone (billed awake)
	// Abort is how long a request that cuts a suspend short waits for the
	// workspace to thaw.
	Abort time.Duration
}

// Cause says why a sandbox call was held.
type Cause string

// Causes of a hold.
const (
	CauseUserMessage Cause = "userMessage" // the first call after a person acted found it waking
	CauseMidTurn     Cause = "midTurn"     // it fell asleep inside a turn
	CauseDeps        Cause = "deps"        // awake, but the dependencies weren't back yet
	CauseAbort       Cause = "abort"       // it was suspending
)

// Hold is a call that waited.
type Hold struct {
	Wait  time.Duration
	Cause Cause
}

// Result is one session under one policy, before prices.
type Result struct {
	Awake, Suspending, Waking, Asleep time.Duration
	// Suspends counts started suspends; each pays for its uploads.
	Suspends, Aborts, Unfinished int
	HintWakes, RequestWakes      int
	// UnusedHints are wakes a hint started for a person's input that no
	// sandbox call followed.
	UnusedHints int
	Holds       []Hold
	// UserMessageWakes are wakes (or aborted suspends) on the way to the
	// first call after a person's input; Hidden ones made it wait nothing.
	UserMessageWakes, Hidden int
	// Delay is how much later the session ended, from all the waiting.
	Delay time.Duration
}

// Wakes is every wake started.
func (r *Result) Wakes() int { return r.HintWakes + r.RequestWakes }

// AwakeBilled is the time a pod and disk existed.
func (r *Result) AwakeBilled() time.Duration { return r.Awake + r.Suspending + r.Waking }

type phase int

const (
	phAwake phase = iota
	phSuspending
	phAsleep
	phWaking
)

type eventKind int

// At the same instant, a call ending comes first, then the turn ending, a
// person's input, and a call starting.
const (
	evToolEnd eventKind = iota
	evTurnEnd
	evInput
	evToolStart
	evEnd
)

type event struct {
	t    int64
	kind eventKind
	i    int // call, turn or input index
}

const none = int64(-1 << 62)

type sim struct {
	tl    Timeline
	pol   policy.Policy
	hints bool
	p     struct{ serve, deps, suspend, abort int64 }

	now     int64 // simulated time: trace time plus delay
	delay   int64
	ph      phase
	phSince int64

	awakeSince, lastActivity, lastTurnStarted, lastTurnEnded int64
	running, held                                            int
	deadline, suspendEnd, servingAt, depsAt, depsReadyAt     int64

	input     int  // the latest person's input, or -1
	inputWoke bool // its hint started a wake or cut a suspend short
	firstDone bool // its first call has run

	res Result
}

// Simulate runs one timeline under a policy. With hints, the harness sends
// a wake-hint at every input a person makes. The sandbox lives from the
// session's first instant until tail after its last.
func Simulate(tl Timeline, pol policy.Policy, hints bool, p Platform, tail time.Duration) (Result, error) {
	if tl.Start > tl.Last {
		return Result{}, nil
	}
	s := &sim{tl: tl, pol: pol, hints: hints, input: -1, deadline: none}
	s.p.serve, s.p.deps = p.WakeServe.Microseconds(), max(p.WakeDeps, p.WakeServe).Microseconds()
	s.p.suspend, s.p.abort = p.Suspend.Microseconds(), p.Abort.Microseconds()

	var evs []event
	for i, c := range tl.Calls {
		// A call ends after it starts, even an instant one.
		evs = append(evs, event{c.S, evToolStart, i}, event{max(c.E, c.S+1), evToolEnd, i})
	}
	for i, t := range tl.Turns {
		evs = append(evs, event{t.End, evTurnEnd, i})
	}
	for i, in := range tl.Inputs {
		evs = append(evs, event{in.T, evInput, i})
	}
	evs = append(evs, event{tl.Last + tail.Microseconds(), evEnd, 0})
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		return a.t < b.t || a.t == b.t && (a.kind < b.kind || a.kind == b.kind && a.i < b.i)
	})

	s.now, s.phSince = tl.Start, tl.Start
	s.awakeSince, s.lastActivity, s.depsReadyAt = tl.Start, tl.Start, tl.Start
	s.lastTurnStarted, s.lastTurnEnded = none, none
	s.decide()
	for _, e := range evs {
		x := e.t + s.delay
		s.advance(x)
		var err error
		switch e.kind {
		case evToolStart:
			err = s.toolStart(e.i)
		case evToolEnd:
			err = s.toolEnd()
		case evTurnEnd:
			s.lastTurnEnded = s.now
			s.decide()
		case evInput:
			s.personActs(e.i)
		case evEnd:
			s.closeInput()
			if s.ph == phSuspending {
				s.res.Unfinished++
			}
			s.setPhase(s.ph, s.now)
		}
		if err != nil {
			return s.res, fmt.Errorf("%s at %d: %w", tl.Key, e.t, err)
		}
	}
	s.res.Delay = time.Duration(s.delay) * time.Microsecond
	return s.res, nil
}

// advance runs the timers due by t: a finished suspend or wake at t
// comes before an event at t, a policy deadline at t after it.
func (s *sim) advance(t int64) {
	for {
		next, which := none, 0
		switch s.ph {
		case phSuspending:
			next, which = s.suspendEnd, 1
		case phWaking:
			next, which = s.servingAt, 2
		case phAwake:
			if s.deadline != none {
				next, which = s.deadline, 3
			}
		}
		if next == none || next > t || which == 3 && next == t {
			break
		}
		s.now = next
		switch which {
		case 1:
			s.setPhase(phAsleep, next)
		case 2:
			s.setPhase(phAwake, next)
			s.awakeSince, s.lastActivity, s.depsReadyAt = next, next, s.depsAt
			s.decide()
		case 3:
			s.deadline = none
			s.decide()
		}
	}
	s.now = t
}

// decide asks the policy, if the workspace is awake, and starts a suspend
// or sets the next deadline.
func (s *sim) decide() {
	s.deadline = none
	if s.ph != phAwake {
		return
	}
	sig := policy.Signals{
		LastActivity: at(s.lastActivity), LastTurnStarted: at(s.lastTurnStarted), LastTurnEnded: at(s.lastTurnEnded),
		HeldRequests: s.held,
	}
	for range s.running {
		sig.Commands = append(sig.Commands, policy.Command{Kind: policy.CommandExec, Command: "tool call"})
	}
	d := s.pol.Decide(policy.Workspace{Key: s.tl.Key, AwakeSince: at(s.awakeSince)}, sig, at(s.now))
	switch {
	case d.Suspend:
		s.setPhase(phSuspending, s.now)
		s.suspendEnd = s.now + s.p.suspend
		s.res.Suspends++
	case !d.SuspendAt.IsZero():
		s.deadline = d.SuspendAt.UnixMicro()
	}
}

func (s *sim) toolStart(i int) error {
	c := s.tl.Calls[i]
	x := s.now
	first := s.input >= 0 && !s.firstDone && s.tl.Inputs[s.input].First == c.S
	ready, cause, woke := x, Cause(""), false
	switch s.ph {
	case phAwake:
		if c.Deps && x < s.depsReadyAt {
			ready, cause = s.depsReadyAt, CauseDeps
		}
	case phSuspending:
		s.abort(x)
		s.res.Aborts++
		ready, cause, woke = s.awakeSince, CauseAbort, true
	case phAsleep:
		s.wake(x)
		s.res.RequestWakes++
		woke = true
		fallthrough
	case phWaking:
		ready, cause = s.servingAt, CauseMidTurn
		if c.Deps {
			ready = s.depsAt
		}
	}
	if first {
		s.firstDone = true
		if s.inputWoke || woke {
			cause = CauseUserMessage
			s.res.UserMessageWakes++
			if ready == x {
				s.res.Hidden++
			}
		}
	}
	if ready > x {
		// The call is held: nothing in the session moves until it runs.
		s.held++
		s.decide()
		s.advance(ready)
		s.held--
		s.res.Holds = append(s.res.Holds, Hold{Wait: time.Duration(ready-x) * time.Microsecond, Cause: cause})
		s.delay += ready - x
	}
	if s.ph != phAwake {
		return fmt.Errorf("call %d runs while the workspace isn't awake", i)
	}
	s.running++
	s.lastActivity = s.now
	s.decide()
	return nil
}

func (s *sim) toolEnd() error {
	if s.running == 0 || s.ph != phAwake {
		return fmt.Errorf("a call ended that wasn't running")
	}
	s.running--
	s.lastActivity = s.now
	s.decide()
	return nil
}

// personActs is a prompt or an answer. Without hints the platform doesn't
// see it; with them, the hint marks a turn started and wakes a sleeping
// workspace or cuts its suspend short.
func (s *sim) personActs(i int) {
	s.closeInput()
	s.input, s.inputWoke, s.firstDone = i, false, false
	if !s.hints {
		return
	}
	s.lastTurnStarted = s.now
	switch s.ph {
	case phAsleep:
		s.wake(s.now)
		s.res.HintWakes++
		s.inputWoke = true
	case phSuspending:
		s.abort(s.now)
		s.res.Aborts++
		s.inputWoke = true
	case phAwake:
		s.decide()
	}
}

// closeInput counts a hint that woke the workspace for nothing.
func (s *sim) closeInput() {
	if s.input >= 0 && s.inputWoke && s.tl.Inputs[s.input].First == -1 {
		s.res.UnusedHints++
	}
	s.input = -1
}

func (s *sim) wake(x int64) {
	s.setPhase(phWaking, x)
	s.servingAt, s.depsAt = x+s.p.serve, x+s.p.deps
	s.deadline = none
}

// abort cuts a suspend short: awake again once it has thawed.
func (s *sim) abort(x int64) {
	s.setPhase(phAwake, x)
	ready := x + s.p.abort
	s.awakeSince, s.lastActivity = ready, ready
	s.deadline = none
}

// setPhase books the time since the last change and moves to p.
func (s *sim) setPhase(p phase, t int64) {
	d := time.Duration(t-s.phSince) * time.Microsecond
	switch s.ph {
	case phAwake:
		s.res.Awake += d
	case phSuspending:
		s.res.Suspending += d
	case phAsleep:
		s.res.Asleep += d
	case phWaking:
		s.res.Waking += d
	}
	s.ph, s.phSince = p, t
}

func at(us int64) time.Time {
	if us == none {
		return time.Time{}
	}
	return time.UnixMicro(us)
}
