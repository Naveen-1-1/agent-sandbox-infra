package ops_test

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) tick()          { c.t = c.t.Add(time.Millisecond) }

func limiter(max, minSuspend int) (*ops.Limiter, *clock) {
	c := &clock{t: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	return &ops.Limiter{Max: max, MinSuspend: minSuspend, Now: c.now}, c
}

func TestAdmitsUpToMax(t *testing.T) {
	l, c := limiter(2, 0)
	for i, want := range []bool{true, true, false} {
		c.tick()
		if got := l.Acquire(fmt.Sprint("w", i), ops.WakeRequest); got != want {
			t.Fatalf("acquire %d = %v, want %v", i, got, want)
		}
	}
	if !l.Acquire("w0", ops.WakeRequest) {
		t.Fatal("a holder asking again must keep its slot")
	}
	l.Release("w0")
	if !l.Acquire("w2", ops.WakeRequest) {
		t.Fatal("the queued waiter didn't get the freed slot")
	}
}

func TestMostUrgentWaiterGoesFirst(t *testing.T) {
	l, c := limiter(1, 0)
	l.Acquire("busy", ops.WakeRequest)
	type waiter struct {
		key   string
		class ops.Class
	}
	waiters := []waiter{{"suspend", ops.Suspend}, {"hint", ops.WakeHint}, {"request-1", ops.WakeRequest}, {"request-2", ops.WakeRequest}}
	for _, w := range waiters {
		c.tick()
		if l.Acquire(w.key, w.class) {
			t.Fatalf("%s started while the only slot was taken", w.key)
		}
	}
	holder := "busy"
	for _, want := range []string{"request-1", "request-2", "hint", "suspend"} {
		l.Release(holder)
		started := ""
		for i := len(waiters) - 1; i >= 0; i-- { // everyone asks again, least urgent first
			if w := waiters[i]; l.Acquire(w.key, w.class) {
				if started != "" {
					t.Fatalf("%s and %s both started in one slot", started, w.key)
				}
				started = w.key
			}
		}
		if started != want {
			t.Fatalf("%q started, want %s", started, want)
		}
		holder = started
		waiters = slices.DeleteFunc(waiters, func(w waiter) bool { return w.key == started })
	}
}

func TestSuspendsKeepAMinimumShare(t *testing.T) {
	l, c := limiter(3, 1)
	for _, k := range []string{"w1", "w2", "w3"} {
		l.Acquire(k, ops.WakeRequest)
	}
	c.tick()
	l.Acquire("w4", ops.WakeRequest)
	c.tick()
	l.Acquire("s1", ops.Suspend)

	// One slot frees with a wake and a suspend waiting: it is the
	// suspend's minimum share, even though wakes are more urgent.
	l.Release("w1")
	if l.Acquire("w4", ops.WakeRequest) {
		t.Fatal("a wake took the slot reserved for a waiting suspend")
	}
	if !l.Acquire("s1", ops.Suspend) {
		t.Fatal("the waiting suspend didn't get its reserved slot")
	}

	// With the minimum share running, further suspends yield to wakes.
	c.tick()
	l.Acquire("s2", ops.Suspend)
	l.Release("w2")
	if l.Acquire("s2", ops.Suspend) {
		t.Fatal("a suspend beyond the minimum share went ahead of a waiting wake")
	}
	if !l.Acquire("w4", ops.WakeRequest) {
		t.Fatal("the waiting wake didn't get the free slot")
	}
}

func TestClassUpgradeMovesAWaiterUp(t *testing.T) {
	l, c := limiter(1, 0)
	l.Acquire("busy", ops.WakeRequest)
	c.tick()
	l.Acquire("hinted", ops.WakeHint)
	c.tick()
	l.Acquire("requested", ops.WakeRequest)
	c.tick()
	l.Acquire("hinted", ops.WakeRequest) // a request is now held for it
	l.Release("busy")
	if l.Acquire("requested", ops.WakeRequest) {
		t.Fatal("the later request went first, though the upgraded wake was queued earlier")
	}
	if !l.Acquire("hinted", ops.WakeRequest) {
		t.Fatal("the upgraded wake didn't start")
	}
}

func TestReleaseKeepsTheQueuePlaceAndForgetDropsIt(t *testing.T) {
	l, c := limiter(1, 0)
	l.Acquire("busy", ops.WakeRequest)
	c.tick()
	l.Acquire("first", ops.WakeRequest)
	c.tick()
	l.Acquire("second", ops.WakeRequest)
	l.Release("first") // a reconcile settling other state must not cost its place
	l.Release("busy")
	if l.Acquire("second", ops.WakeRequest) {
		t.Fatal("Release dropped a waiter's place in the queue")
	}
	l.Forget("first") // it no longer wants to run
	if !l.Acquire("second", ops.WakeRequest) {
		t.Fatal("a forgotten waiter still blocks the queue")
	}
}

func TestForgottenWaitersAndHoldersExpire(t *testing.T) {
	l, c := limiter(1, 0)
	l.WaitTTL, l.RunTTL = time.Second, time.Minute
	l.Acquire("busy", ops.WakeRequest)
	c.tick()
	l.Acquire("deleted", ops.WakeRequest) // queued, then never asks again
	c.tick()
	l.Acquire("next", ops.WakeRequest)
	l.Release("busy")
	c.t = c.t.Add(2 * time.Second)
	l.Acquire("next", ops.WakeRequest) // still refreshing
	if !l.Acquire("next", ops.WakeRequest) {
		t.Fatal("a waiter that went away still blocks the queue")
	}
	c.t = c.t.Add(2 * time.Minute)
	if !l.Acquire("other", ops.WakeRequest) {
		t.Fatal("a holder that stopped reporting still holds its slot")
	}
}

func TestHoldCountsInProgressWork(t *testing.T) {
	l, _ := limiter(1, 0)
	l.Hold("resumed-1", ops.Suspend)
	l.Hold("resumed-2", ops.WakeRequest) // over the limit: it's already happening
	if l.Acquire("new", ops.WakeRequest) {
		t.Fatal("admitted past in-progress work")
	}
	r, w := l.Counts()
	if r["suspend"] != 1 || r["wake_request"] != 1 || w["wake_request"] != 1 {
		t.Fatalf("running %v waiting %v", r, w)
	}
}

func TestConcurrentUseNeverExceedsMax(t *testing.T) {
	l := &ops.Limiter{Max: 4, MinSuspend: 1}
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, class := fmt.Sprint("w", i), ops.Class(i%3)
			for !l.Acquire(key, class) {
				time.Sleep(100 * time.Microsecond)
			}
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			l.Release(key)
		}()
	}
	wg.Wait()
	if peak > 4 {
		t.Fatalf("%d operations ran at once, want at most 4", peak)
	}
}
