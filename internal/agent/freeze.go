package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
)

// Procs lists and signals the processes in the workload container. The
// Linux implementation reads /proc; tests use a fake.
type Procs interface {
	// List returns every live (non-zombie) process ID in the container.
	List() ([]int, error)
	Signal(pid int, sig syscall.Signal) error
}

// ErrNotFrozen means Thaw named an attempt that does not hold the freeze.
var ErrNotFrozen = errors.New("agent: workload is frozen by another attempt")

// Freezer stops every workload process so nothing writes while a snapshot is
// taken. Freezing first closes the API gate (new exec and file writes get
// FROZEN), waits for file writes already in flight, then sends SIGSTOP to
// every other process, rescanning until a full pass finds nothing new so a
// process forked mid-scan is caught too.
type Freezer struct {
	procs    Procs
	activity *Activity
	skip     map[int]bool // the agent itself and PID 1

	mu       sync.Mutex
	frozen   bool
	attempt  string
	stopped  map[int]bool
	lease    *time.Timer
	maxScans int
}

// NewFreezer never stops the PIDs in skip.
func NewFreezer(procs Procs, activity *Activity, skip ...int) *Freezer {
	s := map[int]bool{}
	for _, p := range skip {
		s[p] = true
	}
	return &Freezer{procs: procs, activity: activity, skip: s, stopped: map[int]bool{}, maxScans: 50}
}

// Frozen reports whether the API gate is closed.
func (f *Freezer) Frozen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frozen
}

// Freeze freezes the workload for attempt. If leaseUntil is set and nothing
// renews the freeze by then, the workload thaws itself; a zero leaseUntil
// holds the freeze until the pod is deleted (used once a suspend is
// committed). Freezing again with the same attempt renews the lease.
func (f *Freezer) Freeze(ctx context.Context, attempt string, leaseUntil time.Time) (int, error) {
	f.mu.Lock()
	f.frozen = true
	f.attempt = attempt
	if f.lease != nil {
		f.lease.Stop()
		f.lease = nil
	}
	f.mu.Unlock()

	select {
	case <-f.activity.WritesIdle():
	case <-ctx.Done():
		_ = f.Thaw(attempt)
		return 0, fmt.Errorf("agent: waiting for file writes to finish: %w", ctx.Err())
	}

	n, err := f.stopAll()
	if err != nil {
		_ = f.Thaw(attempt)
		return 0, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !leaseUntil.IsZero() {
		f.lease = time.AfterFunc(time.Until(leaseUntil), func() { _ = f.Thaw(attempt) })
	}
	return n, nil
}

func (f *Freezer) stopAll() (int, error) {
	for range f.maxScans {
		pids, err := f.procs.List()
		if err != nil {
			return 0, err
		}
		found := false
		for _, pid := range pids {
			f.mu.Lock()
			skip := f.skip[pid] || f.stopped[pid]
			f.mu.Unlock()
			if skip {
				continue
			}
			if err := f.procs.Signal(pid, syscall.SIGSTOP); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					continue // exited between listing and signalling
				}
				return 0, fmt.Errorf("agent: stopping pid %d: %w", pid, err)
			}
			f.mu.Lock()
			f.stopped[pid] = true
			f.mu.Unlock()
			found = true
		}
		if !found {
			f.mu.Lock()
			defer f.mu.Unlock()
			return len(f.stopped), nil
		}
	}
	return 0, errors.New("agent: processes kept appearing while freezing")
}

// Thaw resumes every process this freezer stopped and reopens the API gate.
// Thawing when not frozen is a no-op; thawing another attempt's freeze fails.
func (f *Freezer) Thaw(attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.frozen {
		return nil
	}
	if attempt != f.attempt {
		return ErrNotFrozen
	}
	if f.lease != nil {
		f.lease.Stop()
		f.lease = nil
	}
	var firstErr error
	for pid := range f.stopped {
		if err := f.procs.Signal(pid, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) && firstErr == nil {
			firstErr = err
		}
	}
	f.stopped = map[int]bool{}
	f.frozen = false
	f.attempt = ""
	return firstErr
}
