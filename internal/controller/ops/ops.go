// Package ops bounds how many wakes and suspends run at once, and in what
// order they start.
//
// The controller is level-triggered, so the limiter never blocks: a
// reconcile asks whether its workspace may start an operation, and if not
// it is queued and asks again shortly. Slots go to the most urgent waiter:
// a wake someone is waiting for, then a wake a hint predicted, then a
// suspend. Suspends keep a minimum share even while wakes wait, because
// suspends are what free capacity during a storm.
package ops

import (
	"sync"
	"time"
)

// Class is an operation's priority; lower runs first.
type Class int

// Operation classes, most urgent first.
const (
	WakeRequest Class = iota // a request is waiting for the workspace
	WakeHint                 // a hint says a request is coming
	Suspend                  // nobody waits for a suspend
)

func (c Class) String() string {
	switch c {
	case WakeRequest:
		return "wake_request"
	case WakeHint:
		return "wake_hint"
	}
	return "suspend"
}

// ClassNames lists every class's name, for metrics.
var ClassNames = []string{WakeRequest.String(), WakeHint.String(), Suspend.String()}

// Limiter admits operations. The zero value is not usable; set Max.
type Limiter struct {
	// Max operations in flight.
	Max int
	// MinSuspend slots are kept for suspends while wakes wait.
	MinSuspend int
	// A waiter that stops asking for WaitTTL (its workspace was deleted)
	// loses its place; a running operation not reported for RunTTL is
	// assumed finished.
	WaitTTL, RunTTL time.Duration
	Now             func() time.Time

	mu      sync.Mutex
	running map[string]entry
	waiting map[string]entry
}

type entry struct {
	class       Class
	since, seen time.Time
}

func (l *Limiter) init() time.Time {
	if l.running == nil {
		l.running, l.waiting = map[string]entry{}, map[string]entry{}
	}
	if l.WaitTTL <= 0 {
		l.WaitTTL = 10 * time.Second
	}
	if l.RunTTL <= 0 {
		l.RunTTL = 5 * time.Minute
	}
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	for k, e := range l.waiting {
		if now.Sub(e.seen) > l.WaitTTL {
			delete(l.waiting, k)
		}
	}
	for k, e := range l.running {
		if now.Sub(e.seen) > l.RunTTL {
			delete(l.running, k)
		}
	}
	return now
}

// Acquire asks to start an operation of class c on key. It reports true
// when the operation may start (or already holds a slot). Otherwise key is
// queued; asking again keeps its place, and a class change (a hinted wake
// that now has a request waiting) takes effect at once.
func (l *Limiter) Acquire(key string, c Class) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.init()
	if e, ok := l.running[key]; ok {
		e.seen = now
		l.running[key] = e
		return true
	}
	w, ok := l.waiting[key]
	if !ok {
		w.since = now
	}
	w.class, w.seen = c, now
	l.waiting[key] = w
	if len(l.running) >= l.Max || !l.next(key) {
		return false
	}
	delete(l.waiting, key)
	l.running[key] = entry{class: c, since: now, seen: now}
	return true
}

// next reports whether key is the waiter the next free slot goes to.
func (l *Limiter) next(key string) bool {
	var wake, susp string
	var waitingSuspends, runningSuspends int
	for k, e := range l.waiting {
		if e.class == Suspend {
			waitingSuspends++
			if susp == "" || before(e, l.waiting[susp]) {
				susp = k
			}
		} else if wake == "" || before(e, l.waiting[wake]) {
			wake = k
		}
	}
	for _, e := range l.running {
		if e.class == Suspend {
			runningSuspends++
		}
	}
	switch key {
	case susp:
		return wake == "" || runningSuspends < l.MinSuspend
	case wake:
		reserved := max(0, min(l.MinSuspend, waitingSuspends)-runningSuspends)
		return l.Max-len(l.running) > reserved
	}
	return false
}

// before orders waiters: more urgent class first, then first come.
func before(a, b entry) bool {
	if a.class != b.class {
		return a.class < b.class
	}
	return a.since.Before(b.since)
}

// Hold records an operation already in progress, for example one found in
// status after a controller restart, so it counts against the limit. It
// never refuses.
func (l *Limiter) Hold(key string, c Class) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.init()
	delete(l.waiting, key)
	e, ok := l.running[key]
	if !ok {
		e.since = now
	}
	e.class, e.seen = c, now
	l.running[key] = e
}

// Release ends key's operation. A place in the queue is kept.
func (l *Limiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	delete(l.running, key)
}

// Forget drops key entirely: its operation and its place in the queue.
func (l *Limiter) Forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	delete(l.running, key)
	delete(l.waiting, key)
}

// Counts reports operations running and waiting, by class name.
func (l *Limiter) Counts() (running, waiting map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	running, waiting = map[string]int{}, map[string]int{}
	for _, e := range l.running {
		running[e.class.String()]++
	}
	for _, e := range l.waiting {
		waiting[e.class.String()]++
	}
	return running, waiting
}
