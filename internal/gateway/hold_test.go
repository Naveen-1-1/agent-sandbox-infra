package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain (with the leak check) is in api_test.go, which shares this binary.

func holder(perWS, global int, timeout time.Duration) *Holder {
	return &Holder{PerWorkspace: perWS, Global: global, Timeout: timeout}
}

func TestWaitReturnsAtOnceWhenReady(t *testing.T) {
	h := holder(1, 1, time.Second)
	if err := h.Wait(context.Background(), "a", func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if h.Total() != 0 {
		t.Fatal("a ready request was queued")
	}
}

func TestNotifyReleasesEveryWaiter(t *testing.T) {
	h := holder(100, 100, 10*time.Second)
	var ready atomic.Bool
	check := func() (bool, error) { return ready.Load(), nil }

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.Wait(context.Background(), "ws", check)
		}()
	}
	for h.Total() != 50 {
		time.Sleep(time.Millisecond)
	}
	ready.Store(true)
	h.Notify("ws")
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h.Total() != 0 || len(h.Held()) != 0 {
		t.Fatal("waiters left behind")
	}
}

func TestQueuesAreBounded(t *testing.T) {
	h := holder(2, 3, 10*time.Second)
	block := func() (bool, error) { return false, nil }
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, key := range []string{"a", "a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.Wait(ctx, key, block)
		}()
	}
	for h.Total() != 3 {
		time.Sleep(time.Millisecond)
	}
	if err := h.Wait(ctx, "a", block); !errors.Is(err, ErrQueueFull) {
		t.Errorf("third waiter for a: %v, want ErrQueueFull (per-workspace limit)", err)
	}
	if err := h.Wait(ctx, "c", block); !errors.Is(err, ErrQueueFull) {
		t.Errorf("waiter for c: %v, want ErrQueueFull (global limit)", err)
	}
	cancel()
	wg.Wait()
}

func TestTimeoutAndCancellation(t *testing.T) {
	h := holder(10, 10, 30*time.Millisecond)
	var outcomes []string
	var mu sync.Mutex
	h.Observe = func(o string, _ time.Duration) { mu.Lock(); outcomes = append(outcomes, o); mu.Unlock() }
	block := func() (bool, error) { return false, nil }

	if err := h.Wait(context.Background(), "a", block); !errors.Is(err, ErrWakeTimeout) {
		t.Fatalf("err = %v, want ErrWakeTimeout", err)
	}
	h.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.Wait(ctx, "a", block) }()
	for h.Total() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel() // client went away: the waiter must leave now, not at the timeout
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if h.Total() != 0 {
		t.Fatal("cancelled waiter still counted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) != 2 || outcomes[0] != OutcomeTimeout || outcomes[1] != OutcomeCancelled {
		t.Errorf("outcomes = %v", outcomes)
	}
}

func TestReadyErrorEndsTheWait(t *testing.T) {
	h := holder(10, 10, time.Minute)
	boom := errors.New("workspace deleted")
	calls := 0
	err := h.Wait(context.Background(), "a", func() (bool, error) {
		calls++
		if calls > 1 {
			return false, boom
		}
		return false, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the readiness error", err)
	}
}
