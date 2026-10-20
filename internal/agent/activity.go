// Package agent is sbx-agent, PID 1 of the workload container. It serves
// upstream sandboxd's exec and files APIs with our interceptors in front,
// reports activity for the sleep policy, and freezes the workload while a
// snapshot is taken.
package agent

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
)

// Activity records the in-pod signals a sleep policy needs: the last real
// input, commands still running, and open streams by kind. It also counts
// file writes in flight so a freeze can wait for them.
type Activity struct {
	mu       sync.Mutex
	now      func() time.Time
	last     time.Time
	nextID   int
	commands map[string]*signalsv1.Command
	streams  map[signalsv1.StreamKind]int
	writes   int
	idle     chan struct{} // closed when writes drops to zero
}

// NewActivity starts with the agent's start time as the last activity, so a
// freshly woken workspace isn't immediately idle.
func NewActivity(now func() time.Time) *Activity {
	if now == nil {
		now = time.Now
	}
	return &Activity{
		now: now, last: now(),
		commands: map[string]*signalsv1.Command{},
		streams:  map[signalsv1.StreamKind]int{},
	}
}

// Touch records real user or agent input.
func (a *Activity) Touch() {
	a.mu.Lock()
	a.last = a.now()
	a.mu.Unlock()
}

// StartCommand records a running command; call the returned func when it ends.
func (a *Activity) StartCommand(command string, kind signalsv1.CommandKind) (done func()) {
	a.mu.Lock()
	a.nextID++
	id := strconv.Itoa(a.nextID)
	a.last = a.now()
	a.commands[id] = &signalsv1.Command{Id: id, Command: command, Kind: kind, StartedAt: timestamppb.New(a.now())}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.commands, id)
			a.last = a.now()
			a.mu.Unlock()
		})
	}
}

// OpenStream counts an open long-lived connection; call close when it ends.
func (a *Activity) OpenStream(kind signalsv1.StreamKind) (closeFn func()) {
	a.mu.Lock()
	a.streams[kind]++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.streams[kind]--
			if a.streams[kind] <= 0 {
				delete(a.streams, kind)
			}
			a.mu.Unlock()
		})
	}
}

// BeginWrite marks a file write in flight; call the returned func when done.
func (a *Activity) BeginWrite() (done func()) {
	a.mu.Lock()
	a.writes++
	a.last = a.now()
	if a.idle == nil {
		a.idle = make(chan struct{})
	}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.writes--
			if a.writes == 0 && a.idle != nil {
				close(a.idle)
				a.idle = nil
			}
			a.mu.Unlock()
		})
	}
}

// WritesIdle returns a channel that is closed once no file write is in flight.
func (a *Activity) WritesIdle() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writes == 0 {
		c := make(chan struct{})
		close(c)
		return c
	}
	return a.idle
}

// Signals returns a snapshot of the current signals.
func (a *Activity) Signals() *signalsv1.Signals {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &signalsv1.Signals{LastActivity: timestamppb.New(a.last)}
	for _, c := range a.commands {
		s.RunningCommands = append(s.RunningCommands, c)
	}
	sort.Slice(s.RunningCommands, func(i, j int) bool { return s.RunningCommands[i].Id < s.RunningCommands[j].Id })
	for k, n := range a.streams {
		s.OpenStreams = append(s.OpenStreams, &signalsv1.Stream{Kind: k, Count: int32(n)}) //nolint:gosec // small counts
	}
	sort.Slice(s.OpenStreams, func(i, j int) bool { return s.OpenStreams[i].Kind < s.OpenStreams[j].Kind })
	return s
}
