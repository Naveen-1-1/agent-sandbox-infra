//go:build e2e

package e2e

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// gwWorkspace creates a workspace through the gateway and waits for it.
func gwWorkspace(t *testing.T, gw *sbxapi.Client, p framework.Platform) (ns, name string, own *sbxapi.Client) {
	t.Helper()
	ctx := framework.Ctx(t, 5*time.Minute)
	resp, err := gw.Create(ctx, sbxapi.CreateRequest{RepoURL: p.FixturesURL + "/hello-node.git", Image: "debian:trixie-slim"})
	if err != nil {
		t.Fatal(err)
	}
	ns, name = resp.Workspace.Namespace, resp.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	own = &sbxapi.Client{BaseURL: gw.BaseURL, Token: resp.Token, HTTP: gw.HTTP}
	waitPhase(t, own, ns, name, "Running", 4*time.Minute)
	return ns, name, own
}

func waitPhase(t *testing.T, c *sbxapi.Client, ns, name, phase string, timeout time.Duration) *sbxapi.Workspace {
	t.Helper()
	var w *sbxapi.Workspace
	framework.Eventually(t, timeout, 250*time.Millisecond, ns+"/"+name+" "+phase, func() (bool, error) {
		var err error
		w, err = c.Get(context.Background(), ns, name)
		return err == nil && w.Phase == phase, err
	})
	return w
}

func suspendAndWait(t *testing.T, c *sbxapi.Client, ns, name string) *sbxapi.Workspace {
	t.Helper()
	if err := c.Suspend(framework.Ctx(t, time.Minute), ns, name); err != nil {
		t.Fatal(err)
	}
	return waitPhase(t, c, ns, name, "Suspended", 3*time.Minute)
}

// TestGatewayStormWakesOnce sends 600 commands and 50 wake-hints at a
// sleeping workspace through both gateway replicas at once: exactly one wake
// happens, requests beyond each replica's hold limit get a retryable 503,
// and every held request runs once the workspace is up.
func TestGatewayStormWakesOnce(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p)
	before := suspendAndWait(t, own, ns, name).Epoch

	var ok, full, other atomic.Int64
	var wg sync.WaitGroup
	for i := range 650 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := framework.Ctx(t, 3*time.Minute)
			if i%13 == 0 {
				if err := own.WakeHint(ctx, ns, name); err != nil {
					other.Add(1)
				}
				return
			}
			out, err := own.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"echo", "ok"}})
			var se *sbxapi.StatusError
			switch {
			case err == nil && out.Stdout == "ok\n":
				ok.Add(1)
			case errors.As(err, &se) && se.Body.Code == sbxapi.CodeQueueFull:
				full.Add(1)
			default:
				other.Add(1)
				t.Logf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	after, err := own.Get(framework.Ctx(t, time.Minute), ns, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("served %d, queue full %d, other %d; epoch %d -> %d", ok.Load(), full.Load(), other.Load(), before, after.Epoch)
	if after.Epoch != before+1 {
		t.Errorf("epoch went %d -> %d; want exactly one wake", before, after.Epoch)
	}
	if ok.Load() == 0 || other.Load() != 0 {
		t.Errorf("want held requests served and no other failures")
	}
	if full.Load() == 0 {
		t.Errorf("no request hit the hold limit; the storm was too small to test backpressure")
	}
}

// TestStatusAndHealthNeverWake checks that reading status and the gateway's
// health and metrics endpoints leave a sleeping workspace asleep.
func TestStatusAndHealthNeverWake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p)
	suspendAndWait(t, own, ns, name)

	admin := framework.PortForward(t, framework.Namespace(), "svc/sbx-gateway-signals", 8081)
	for range 5 {
		if _, err := own.Get(framework.Ctx(t, time.Minute), ns, name); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			resp, err := gw.HTTP.Get("http://" + admin + path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	time.Sleep(2 * time.Second)
	if w, _ := own.Get(framework.Ctx(t, time.Minute), ns, name); w.Phase != "Suspended" {
		t.Fatalf("phase = %s after status and health requests, want Suspended", w.Phase)
	}
}

// TestWakeHintHidesTheWake: a user message arrives (wake-hint), the agent
// "thinks", and by the time its first command runs the workspace is awake.
func TestWakeHintHidesTheWake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p)
	suspendAndWait(t, own, ns, name)

	if err := own.WakeHint(framework.Ctx(t, time.Minute), ns, name); err != nil {
		t.Fatal(err)
	}
	time.Sleep(12 * time.Second) // the LLM's thinking time
	out, err := own.Exec(framework.Ctx(t, 2*time.Minute), ns, name, sbxapi.ExecRequest{Command: []string{"cat", "README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first command after the hint waited %dms", out.WaitedMillis)
	if out.WaitedMillis > 1000 {
		t.Errorf("the wake was visible: waited %dms after 12s of thinking", out.WaitedMillis)
	}
}

// TestRequestDuringSuspendIsServed sends a command while a suspend is in
// progress. Before the commit point it aborts the suspend; after it, the
// workspace wakes right after. Either way the command runs and no work is lost.
func TestRequestDuringSuspendIsServed(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p)
	ctx := framework.Ctx(t, 5*time.Minute)
	if _, err := own.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"sh", "-c",
		"echo keep > keep.txt && head -c 20000000 /dev/urandom | base64 > big.txt"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := own.Get(ctx, ns, name)
	if err := own.Suspend(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	framework.Eventually(t, time.Minute, 20*time.Millisecond, "suspend started", func() (bool, error) {
		w, err := own.Get(ctx, ns, name)
		return err == nil && (w.Phase == "Suspending" || w.Phase == "TearingDown"), err
	})
	out, err := own.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"cat", "keep.txt"}})
	if err != nil || strings.TrimSpace(out.Stdout) != "keep" {
		t.Fatalf("command during the suspend: %+v, %v", out, err)
	}
	after, _ := own.Get(ctx, ns, name)
	switch after.Epoch {
	case before.Epoch:
		t.Logf("the request aborted the suspend (waited %dms)", out.WaitedMillis)
	case before.Epoch + 1:
		t.Logf("the suspend had committed; the request woke it (waited %dms)", out.WaitedMillis)
	default:
		t.Errorf("epoch %d -> %d", before.Epoch, after.Epoch)
	}
	if after.Phase != "Running" {
		t.Errorf("phase = %s, want Running", after.Phase)
	}

}
