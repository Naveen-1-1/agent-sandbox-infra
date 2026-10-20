package leaktest_test

import (
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

// fakeTB records failures instead of failing the real test.
type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Error(args ...any)     { f.failed = true }
func (f *fakeTB) Errorf(string, ...any) { f.failed = true }

func TestCheckDetectsLeak(t *testing.T) {
	stop := make(chan struct{})
	go func() { <-stop }()

	tb := &fakeTB{TB: t}
	leaktest.Check(tb)
	close(stop)
	if !tb.failed {
		t.Fatal("Check did not report a leaked goroutine")
	}
}

func TestCheckPassesWhenClean(t *testing.T) {
	done := make(chan struct{})
	go func() { close(done) }()
	<-done

	tb := &fakeTB{TB: t}
	leaktest.Check(tb)
	if tb.failed {
		t.Fatal("Check reported a leak when there was none")
	}
}
