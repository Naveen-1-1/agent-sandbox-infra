package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProcs is a process table. onStop, if set, runs after each SIGSTOP and
// can add processes, as a fork racing the freeze would.
type fakeProcs struct {
	mu      sync.Mutex
	pids    map[int]string // pid -> "running" | "stopped"
	onStop  func(pid int)
	signals []string
}

func newFakeProcs(pids ...int) *fakeProcs {
	p := &fakeProcs{pids: map[int]string{}}
	for _, pid := range pids {
		p.pids[pid] = "running"
	}
	return p
}

func (p *fakeProcs) List() ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for pid := range p.pids {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out, nil
}

func (p *fakeProcs) Signal(pid int, sig syscall.Signal) error {
	p.mu.Lock()
	if _, ok := p.pids[pid]; !ok {
		p.mu.Unlock()
		return syscall.ESRCH
	}
	switch sig {
	case syscall.SIGSTOP:
		p.pids[pid] = "stopped"
	case syscall.SIGCONT:
		p.pids[pid] = "running"
	}
	p.signals = append(p.signals, sig.String())
	hook := p.onStop
	p.mu.Unlock()
	if hook != nil && sig == syscall.SIGSTOP {
		hook(pid)
	}
	return nil
}

func (p *fakeProcs) state(pid int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pids[pid]
}

func TestFreezeStopsEverythingButTheAgent(t *testing.T) {
	procs := newFakeProcs(1, 7, 20, 21, 22)
	f := NewFreezer(procs, NewActivity(nil), 1, 7)

	n, err := f.Freeze(context.Background(), "a1", time.Time{})
	if err != nil || n != 3 {
		t.Fatalf("Freeze = %d, %v; want 3 stopped", n, err)
	}
	if !f.Frozen() {
		t.Fatal("not frozen")
	}
	for _, pid := range []int{20, 21, 22} {
		if procs.state(pid) != "stopped" {
			t.Errorf("pid %d is %s, want stopped", pid, procs.state(pid))
		}
	}
	for _, pid := range []int{1, 7} {
		if procs.state(pid) != "running" {
			t.Errorf("agent pid %d was stopped", pid)
		}
	}
	if err := f.Thaw("a1"); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{20, 21, 22} {
		if procs.state(pid) != "running" {
			t.Errorf("pid %d is %s after thaw, want running", pid, procs.state(pid))
		}
	}
	if f.Frozen() {
		t.Error("still frozen after thaw")
	}
}

func TestFreezeCatchesProcessesForkedMidScan(t *testing.T) {
	procs := newFakeProcs(1, 20)
	forked := false
	procs.onStop = func(pid int) {
		if pid == 20 && !forked {
			forked = true
			procs.mu.Lock()
			procs.pids[30] = "running" // forked before 20 stopped
			procs.mu.Unlock()
		}
	}
	f := NewFreezer(procs, NewActivity(nil), 1)
	if _, err := f.Freeze(context.Background(), "a1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if procs.state(30) != "stopped" {
		t.Errorf("forked pid 30 is %s, want stopped", procs.state(30))
	}
}

func TestLeaseThawsAndZeroLeaseHolds(t *testing.T) {
	procs := newFakeProcs(1, 20)
	f := NewFreezer(procs, NewActivity(nil), 1)
	if _, err := f.Freeze(context.Background(), "a1", time.Now().Add(30*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.Frozen() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Frozen() || procs.state(20) != "running" {
		t.Fatal("lease expired but the workload is still frozen")
	}

	// Committed suspend: freeze without a lease and nothing thaws it.
	if _, err := f.Freeze(context.Background(), "a2", time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if !f.Frozen() {
		t.Fatal("a freeze without a lease thawed by itself")
	}
	_ = f.Thaw("a2")
}

func TestRefreezeRenewsLease(t *testing.T) {
	procs := newFakeProcs(1, 20)
	f := NewFreezer(procs, NewActivity(nil), 1)
	if _, err := f.Freeze(context.Background(), "a1", time.Now().Add(40*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Freeze(context.Background(), "a1", time.Time{}); err != nil { // commit: hold
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if !f.Frozen() {
		t.Fatal("the first lease thawed a freeze that was renewed without a lease")
	}
	_ = f.Thaw("a1")
}

func TestThawRequiresTheFreezingAttempt(t *testing.T) {
	f := NewFreezer(newFakeProcs(1, 20), NewActivity(nil), 1)
	if err := f.Thaw("nobody"); err != nil {
		t.Fatalf("thaw while not frozen: %v, want nil", err)
	}
	if _, err := f.Freeze(context.Background(), "a1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Thaw("a2"); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("thaw by another attempt: %v, want ErrNotFrozen", err)
	}
	if err := f.Thaw("a1"); err != nil {
		t.Fatal(err)
	}
}

func TestFreezeWaitsForWritesInFlight(t *testing.T) {
	a := NewActivity(nil)
	f := NewFreezer(newFakeProcs(1), a, 1)
	done := a.BeginWrite()

	frozen := make(chan error, 1)
	go func() {
		_, err := f.Freeze(context.Background(), "a1", time.Time{})
		frozen <- err
	}()
	select {
	case err := <-frozen:
		t.Fatalf("Freeze returned (%v) while a write was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	done()
	if err := <-frozen; err != nil {
		t.Fatal(err)
	}
	_ = f.Thaw("a1")
}

func TestFreezeGivesUpWhenWritesNeverFinish(t *testing.T) {
	a := NewActivity(nil)
	f := NewFreezer(newFakeProcs(1), a, 1)
	done := a.BeginWrite()
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := f.Freeze(ctx, "a1", time.Time{}); err == nil {
		t.Fatal("Freeze succeeded with a write stuck in flight")
	}
	if f.Frozen() {
		t.Fatal("a failed freeze left the gate closed")
	}
}
