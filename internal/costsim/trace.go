// Package costsim replays real coding-agent sessions (the TraceLab traces)
// against sleep policies, to price what sleeping saves and what it costs in
// waits. Each session's timeline is rebuilt from its timestamps: when tools
// ran in the sandbox, when the model was thinking, when a person was
// replying. A simulator then runs the platform's real policy over it, with
// wake and suspend times measured by the benchmark.
package costsim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Session is one raw session as exported from the trace (one JSON line).
// Times are microseconds since the epoch.
type Session struct {
	Key      string `json:"key"`      // provider:session id
	Provider string `json:"provider"` // claude or codex
	Session  string `json:"session"`
	// Root is the key of the session it belongs to: its own for a top-level
	// session, its parent's for a subagent.
	Root       string  `json:"root"`
	User       string  `json:"user"`
	Project    string  `json:"project"`
	Rounds     int     `json:"rounds"`
	UserRounds int     `json:"userRounds"`
	Events     []Event `json:"events"`
	Tools      []Tool  `json:"tools"`
}

// Event is an instant: a message, reasoning or text.
type Event struct {
	T int64 `json:"t"`
	// User is a user-role message (a prompt, or a meta or command record).
	User bool `json:"u"`
	// Human is the user message that started a user-initiated round: a
	// person's prompt.
	Human bool `json:"h"`
}

// Tool is one tool call.
type Tool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	S    int64  `json:"s"` // emitted
	E    *int64 `json:"e"` // result; nil if it never returned
	// InternalMs is the tool's own reported duration, if any.
	InternalMs *float64 `json:"internalMs"`
	// Exe lists the programs a shell call ran, where the trace says.
	Exe []string `json:"exe"`
}

// IsSubagent reports whether s belongs to another session.
func (s *Session) IsSubagent() bool { return s.Root != s.Key }

// Read reads the exported sessions, one JSON object per line.
func Read(r io.Reader) ([]Session, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	var out []Session
	for {
		var s Session
		err := dec.Decode(&s)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("costsim: session %d: %w", len(out)+1, err)
		}
		if s.Key == "" || s.Root == "" {
			return nil, fmt.Errorf("costsim: session %d has no key or root", len(out)+1)
		}
		out = append(out, s)
	}
}
