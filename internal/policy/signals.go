package policy

import (
	"time"

	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
)

// CommandKind says how a running command was started.
type CommandKind string

// Command kinds, as written in policy parameters.
const (
	CommandExec     CommandKind = "exec"     // the exec API, usually the agent
	CommandTerminal CommandKind = "terminal" // an editor terminal
	CommandService  CommandKind = "service"  // a declared long-running service
)

// StreamKind classifies a long-lived connection.
type StreamKind string

// Stream kinds, as written in policy parameters.
const (
	StreamExec   StreamKind = "exec"   // an exec session's output
	StreamEditor StreamKind = "editor" // the editor's websocket
	StreamApp    StreamKind = "app"    // a connection to an app port
)

// Command is a running command.
type Command struct {
	Kind      CommandKind
	Command   string
	StartedAt time.Time
}

// Signals is everything known about a workspace's use, merged from every
// source (its agent, each gateway replica, the controller).
type Signals struct {
	LastActivity    time.Time
	LastTurnStarted time.Time
	LastTurnEnded   time.Time
	Commands        []Command
	Streams         map[StreamKind]int
	HeldRequests    int
	// Unknown names sources that couldn't be read. A policy must not
	// suspend while any is unknown.
	Unknown []string
}

// Merge adds o's view to s: the latest times, all commands, summed counts.
func (s *Signals) Merge(o Signals) {
	s.LastActivity = latest(s.LastActivity, o.LastActivity)
	s.LastTurnStarted = latest(s.LastTurnStarted, o.LastTurnStarted)
	s.LastTurnEnded = latest(s.LastTurnEnded, o.LastTurnEnded)
	s.Commands = append(s.Commands, o.Commands...)
	for k, n := range o.Streams {
		if s.Streams == nil {
			s.Streams = map[StreamKind]int{}
		}
		s.Streams[k] += n
	}
	s.HeldRequests += o.HeldRequests
	s.Unknown = append(s.Unknown, o.Unknown...)
}

// FromProto converts one source's report.
func FromProto(p *signalsv1.Signals) Signals {
	s := Signals{HeldRequests: int(p.GetHeldRequests())}
	if t := p.GetLastActivity(); t != nil {
		s.LastActivity = t.AsTime()
	}
	if t := p.GetLastTurnStarted(); t != nil {
		s.LastTurnStarted = t.AsTime()
	}
	if t := p.GetLastTurnEnded(); t != nil {
		s.LastTurnEnded = t.AsTime()
	}
	for _, c := range p.GetRunningCommands() {
		cmd := Command{Kind: commandKind(c.GetKind()), Command: c.GetCommand()}
		if t := c.GetStartedAt(); t != nil {
			cmd.StartedAt = t.AsTime()
		}
		s.Commands = append(s.Commands, cmd)
	}
	for _, st := range p.GetOpenStreams() {
		if st.GetCount() > 0 {
			if s.Streams == nil {
				s.Streams = map[StreamKind]int{}
			}
			s.Streams[streamKind(st.GetKind())] += int(st.GetCount())
		}
	}
	return s
}

func commandKind(k signalsv1.CommandKind) CommandKind {
	switch k {
	case signalsv1.CommandKind_COMMAND_KIND_TERMINAL:
		return CommandTerminal
	case signalsv1.CommandKind_COMMAND_KIND_SERVICE:
		return CommandService
	}
	return CommandExec // unspecified counts as the strictest common kind
}

func streamKind(k signalsv1.StreamKind) StreamKind {
	switch k {
	case signalsv1.StreamKind_STREAM_KIND_EDITOR:
		return StreamEditor
	case signalsv1.StreamKind_STREAM_KIND_APP:
		return StreamApp
	}
	return StreamExec
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
