// Package leaktest makes every test package fail when it leaks goroutines.
//
// Each package with tests calls VerifyTestMain from its TestMain:
//
//	func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }
//
// A guard test in this package fails the build when a test package forgets.
package leaktest

import (
	"testing"

	"go.uber.org/goleak"
)

// ignored lists long-lived goroutines started by libraries we depend on.
// They are process-wide singletons, not leaks in our code.
var ignored = []goleak.Option{
	goleak.IgnoreTopFunction("k8s.io/klog/v2.(*flushDaemon).run.func1"),
	goleak.IgnoreTopFunction("k8s.io/klog/v2.(*loggingT).flushDaemon"),
	goleak.IgnoreAnyFunction("k8s.io/client-go/util/workqueue.(*Typed[...]).updateUnfinishedWorkLoop"),
	goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
}

// VerifyTestMain runs the package's tests and then fails if any goroutine
// started by them is still running.
func VerifyTestMain(m goleak.TestingM, opts ...goleak.Option) {
	goleak.VerifyTestMain(m, append(append([]goleak.Option{}, ignored...), opts...)...)
}

// WithTeardown runs teardown after the tests and before the leak check, for
// package-wide fixtures such as a test API server.
func WithTeardown(m *testing.M, teardown func()) goleak.TestingM {
	return teardownM{m: m, teardown: teardown}
}

type teardownM struct {
	m        *testing.M
	teardown func()
}

func (t teardownM) Run() int {
	code := t.m.Run()
	t.teardown()
	return code
}

// Check fails t if goroutines leak from a single test; use it in tests that
// need a tighter scope than the whole package.
func Check(t testing.TB, opts ...goleak.Option) {
	t.Helper()
	goleak.VerifyNone(t, append(append([]goleak.Option{}, ignored...), opts...)...)
}
