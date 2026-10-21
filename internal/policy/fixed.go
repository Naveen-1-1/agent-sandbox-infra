package policy

import (
	"fmt"
	"strings"
	"time"
)

// How soon to look again when something holds a workspace awake.
const (
	vetoRecheck    = 15 * time.Second
	unknownRecheck = 10 * time.Second
)

func init() {
	Register(Spec{
		Name: "fixed",
		Help: "Suspend after a fixed time without activity, or sooner once the agent ends its turn.",
		Params: []Param{
			{Name: "idleTimeout", Default: "10m", Help: "Suspend after this long without activity."},
			{Name: "afterTurnEnded", Default: "2m", Help: "Suspend this soon after the agent ends its turn if nothing happens since; 0 turns it off."},
			{Name: "blockingCommands", Default: "exec,terminal", Help: "Running commands of these kinds keep the workspace awake (exec, terminal, service)."},
			{Name: "blockingStreams", Default: "exec,app", Help: "Open connections of these kinds keep the workspace awake (exec, editor, app)."},
		},
		New: newFixed,
	})
	Register(Spec{
		Name: "never",
		Help: "Never suspend on its own; only explicit suspends.",
		New:  func(map[string]string) (Policy, error) { return never{}, nil },
	})
}

type fixed struct {
	idle, afterTurn time.Duration
	commands        map[CommandKind]bool
	streams         map[StreamKind]bool
}

func newFixed(p map[string]string) (Policy, error) {
	f := &fixed{commands: map[CommandKind]bool{}, streams: map[StreamKind]bool{}}
	var err error
	if f.idle, err = time.ParseDuration(p["idleTimeout"]); err != nil || f.idle < 5*time.Second {
		return nil, fmt.Errorf("idleTimeout %q: want a duration of at least 5s", p["idleTimeout"])
	}
	if f.afterTurn, err = parseDurationOrZero(p["afterTurnEnded"]); err != nil || f.afterTurn < 0 {
		return nil, fmt.Errorf("afterTurnEnded %q: want a duration, or 0 for off", p["afterTurnEnded"])
	}
	for _, k := range list(p["blockingCommands"]) {
		switch CommandKind(k) {
		case CommandExec, CommandTerminal, CommandService:
			f.commands[CommandKind(k)] = true
		default:
			return nil, fmt.Errorf("blockingCommands: unknown kind %q", k)
		}
	}
	for _, k := range list(p["blockingStreams"]) {
		switch StreamKind(k) {
		case StreamExec, StreamEditor, StreamApp:
			f.streams[StreamKind(k)] = true
		default:
			return nil, fmt.Errorf("blockingStreams: unknown kind %q", k)
		}
	}
	return f, nil
}

func (*fixed) Name() string { return "fixed" }

func (f *fixed) Decide(ws Workspace, s Signals, now time.Time) Decision {
	if len(s.Unknown) > 0 {
		return Decision{Reason: ReasonSignalsUnknown, RecheckAfter: unknownRecheck,
			Message: "can't read " + strings.Join(s.Unknown, ", ") + "; staying up"}
	}
	if s.HeldRequests > 0 {
		return Decision{Reason: ReasonRequestsHeld, RecheckAfter: vetoRecheck,
			Message: fmt.Sprintf("%d requests are waiting", s.HeldRequests)}
	}
	for _, c := range s.Commands {
		if f.commands[c.Kind] {
			return Decision{Reason: ReasonCommandRunning, RecheckAfter: vetoRecheck,
				Message: fmt.Sprintf("a %s command is running: %s", c.Kind, c.Command)}
		}
	}
	for k, n := range s.Streams {
		if n > 0 && f.streams[k] {
			return Decision{Reason: ReasonStreamOpen, RecheckAfter: vetoRecheck,
				Message: fmt.Sprintf("%d %s connections are open", n, k)}
		}
	}

	// A turn that started (a user message) counts as activity: the agent
	// is thinking and will soon need the workspace.
	last := latest(latest(ws.AwakeSince, s.LastActivity), s.LastTurnStarted)
	at, reason := last.Add(f.idle), ReasonIdle
	done := fmt.Sprintf("no activity for %s", f.idle)
	pending := fmt.Sprintf("suspends after %s without activity", f.idle)
	if f.afterTurn > 0 && !s.LastTurnEnded.IsZero() && !s.LastTurnEnded.Before(last) {
		if t := s.LastTurnEnded.Add(f.afterTurn); t.Before(at) {
			at, reason = t, ReasonTurnEnded
			done = fmt.Sprintf("the agent's turn ended and nothing happened for %s", f.afterTurn)
			pending = fmt.Sprintf("the agent's turn ended; suspends after %s unless it continues", f.afterTurn)
		}
	}
	if !now.Before(at) {
		return Decision{Suspend: true, Reason: reason, Message: done}
	}
	// The message stays the same while the timer runs; SuspendAt says when.
	return Decision{Reason: ReasonActive, SuspendAt: at, RecheckAfter: at.Sub(now), Message: pending}
}

type never struct{}

func (never) Name() string { return "never" }

func (never) Decide(Workspace, Signals, time.Time) Decision {
	return Decision{Reason: ReasonNever, Message: "the policy never suspends on its own"}
}

func parseDurationOrZero(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
