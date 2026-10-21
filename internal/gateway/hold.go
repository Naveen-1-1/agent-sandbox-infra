// Package gateway is sbx-gateway: every request to a workspace goes through
// it. It authenticates the caller, holds requests to a sleeping workspace
// while it wakes, then forwards them to the workspace's pod.
package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrQueueFull means too many requests already wait for this workspace
	// (or for all workspaces); the caller should retry later.
	ErrQueueFull = errors.New("gateway: too many requests waiting")
	// ErrWakeTimeout means the workspace did not become ready in time.
	ErrWakeTimeout = errors.New("gateway: workspace did not wake in time")
)

// Holder parks requests until their workspace is ready. Waiters re-check
// readiness whenever the workspace changes (Notify), so nothing polls hard;
// a slow fallback tick covers a missed notification. Both the number of
// waiters per workspace and in total are bounded, and a waiter leaves as soon
// as its request is cancelled.
type Holder struct {
	PerWorkspace int
	Global       int
	Timeout      time.Duration
	// Observe, if set, is told how each wait ended and how long it took.
	Observe func(outcome string, waited time.Duration)

	mu    sync.Mutex
	queue map[string]*waitList
	total int
}

type waitList struct {
	n      int
	change chan struct{} // closed and replaced on every Notify
}

// Wait outcomes reported to Observe.
const (
	OutcomeReady     = "ready"
	OutcomeTimeout   = "timeout"
	OutcomeCancelled = "cancelled"
	OutcomeError     = "error"
)

// Held returns how many requests are waiting, per workspace.
func (h *Holder) Held() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.queue))
	for k, w := range h.queue {
		out[k] = w.n
	}
	return out
}

// Total returns how many requests are waiting.
func (h *Holder) Total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

// Notify wakes every waiter for key to re-check readiness.
func (h *Holder) Notify(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w, ok := h.queue[key]; ok {
		close(w.change)
		w.change = make(chan struct{})
	}
}

// Wait returns once ready reports true, or with ErrQueueFull, ErrWakeTimeout,
// the context's error, or ready's error.
func (h *Holder) Wait(ctx context.Context, key string, ready func() (bool, error)) error {
	if ok, err := ready(); ok || err != nil {
		return err
	}
	w, err := h.join(key)
	if err != nil {
		return err
	}
	start := time.Now()
	outcome := OutcomeError
	defer func() {
		h.leave(key)
		if h.Observe != nil {
			h.Observe(outcome, time.Since(start))
		}
	}()

	timeout := time.NewTimer(h.Timeout)
	defer timeout.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		h.mu.Lock()
		change := w.change // take it before checking, so a Notify in between isn't missed
		h.mu.Unlock()
		ok, err := ready()
		if err != nil {
			return err
		}
		if ok {
			outcome = OutcomeReady
			return nil
		}
		select {
		case <-change:
		case <-tick.C:
		case <-ctx.Done():
			outcome = OutcomeCancelled
			return ctx.Err()
		case <-timeout.C:
			outcome = OutcomeTimeout
			return ErrWakeTimeout
		}
	}
}

func (h *Holder) join(key string) (*waitList, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.queue == nil {
		h.queue = map[string]*waitList{}
	}
	w := h.queue[key]
	if h.total >= h.Global || (w != nil && w.n >= h.PerWorkspace) {
		return nil, ErrQueueFull
	}
	if w == nil {
		w = &waitList{change: make(chan struct{})}
		h.queue[key] = w
	}
	w.n++
	h.total++
	return w, nil
}

func (h *Holder) leave(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.queue[key]
	w.n--
	h.total--
	if w.n == 0 {
		delete(h.queue, key)
	}
}
