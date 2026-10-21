//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

func fixedPolicy(kv ...string) *sbxapi.PolicyRef {
	p := &sbxapi.PolicyRef{Name: "fixed", Params: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		p.Params[kv[i]] = kv[i+1]
	}
	return p
}

// TestIdleWorkspaceSuspendsItself: with no activity, the policy suspends the
// workspace after its idle timeout and says so.
func TestIdleWorkspaceSuspendsItself(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p, fixedPolicy("idleTimeout", "15s", "afterTurnEnded", "0"))

	w := waitSleep(t, own, ns, name, "Active")
	if w.Sleep.SuspendAt == nil || time.Until(*w.Sleep.SuspendAt) > 15*time.Second {
		t.Fatalf("sleep = %+v, want a suspend time within the idle timeout", w.Sleep)
	}
	w = waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)
	if w.Sleep == nil || w.Sleep.Policy != "fixed" || w.Sleep.Reason != "Idle" {
		t.Fatalf("suspended with sleep = %+v, want fixed/Idle", w.Sleep)
	}
}

// TestRunningCommandKeepsItAwake: a command that runs past the idle timeout
// holds the workspace up; once it ends, the timer starts again.
func TestRunningCommandKeepsItAwake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p, fixedPolicy("idleTimeout", "10s", "afterTurnEnded", "0"))

	done := make(chan error, 1)
	go func() {
		_, err := own.Exec(context.Background(), ns, name, sbxapi.ExecRequest{Command: []string{"sleep", "35"}})
		done <- err
	}()
	waitSleep(t, own, ns, name, "CommandRunning")
	deadline := time.After(25 * time.Second) // well past the idle timeout
	for waiting := true; waiting; {
		select {
		case err := <-done:
			t.Fatalf("the command ended early: %v", err)
		case <-deadline:
			waiting = false
		case <-time.After(time.Second):
			if w, err := own.Get(context.Background(), ns, name); err != nil || w.Phase != "Running" {
				t.Fatalf("phase %s while a command runs (%v)", w.Phase, err)
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)
}

// TestTurnEndedSuspendsSooner: when the agent ends its turn, the workspace
// sleeps after the short afterTurnEnded delay rather than the idle timeout.
func TestTurnEndedSuspendsSooner(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := gwWorkspace(t, gw, p, fixedPolicy("idleTimeout", "30m", "afterTurnEnded", "5s"))
	ctx := framework.Ctx(t, 3*time.Minute)
	if _, err := own.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"echo", "done"}}); err != nil {
		t.Fatal(err)
	}
	if err := own.TurnEnded(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	ended := time.Now()
	w := waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)
	if w.Sleep == nil || w.Sleep.Reason != "TurnEnded" {
		t.Fatalf("sleep = %+v, want TurnEnded", w.Sleep)
	}
	t.Logf("suspended %s after the turn ended", time.Since(ended).Round(time.Second))
}

// TestClusterPolicyHotSwap writes the runtime policy ConfigMap: running
// workspaces follow it at once; a broken edit is reported and ignored.
func TestClusterPolicyHotSwap(t *testing.T) {
	gw := framework.Gateway(t)
	c := framework.NewClients(t)
	p := framework.DeployedPlatform(t, c)
	ns, name, own := gwWorkspace(t, gw, p, nil)
	w := waitSleep(t, own, ns, name, "Active")
	if w.Sleep.Policy != "fixed" || w.Sleep.SuspendAt == nil || time.Until(*w.Sleep.SuspendAt) < 5*time.Minute {
		t.Fatalf("under the chart's default policy: %+v, want fixed with minutes to go", w.Sleep)
	}

	ctx := framework.Ctx(t, 5*time.Minute)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: framework.Namespace(), Name: sleep.PolicyConfigMap},
		Data:       map[string]string{policy.ConfigKey: "name: fixed\nparams:\n  idleTimeout: 10s\n  afterTurnEnded: \"0\"\n"},
	}
	_ = c.CR.Delete(ctx, cm)
	if err := c.CR.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CR.Delete(context.Background(), cm) })
	waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)

	cm.Data[policy.ConfigKey] = "name: fixed\nparams:\n  idleTimeout: whenever\n"
	if err := c.CR.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	framework.Eventually(t, time.Minute, time.Second, "an InvalidPolicy warning", func() (bool, error) {
		evs := &eventsv1.EventList{}
		if err := c.CR.List(ctx, evs, client.InNamespace(framework.Namespace())); err != nil {
			return false, err
		}
		for _, e := range evs.Items {
			if e.Regarding.Name == sleep.PolicyConfigMap && e.Reason == "InvalidPolicy" && strings.Contains(e.Note, "whenever") {
				return true, nil
			}
		}
		return false, nil
	})
	// The previous policy (10s) still applies after a wake.
	if _, err := own.Exec(ctx, ns, name, sbxapi.ExecRequest{Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)
}

func waitSleep(t *testing.T, c *sbxapi.Client, ns, name, reason string) *sbxapi.Workspace {
	t.Helper()
	var w *sbxapi.Workspace
	framework.Eventually(t, 2*time.Minute, 250*time.Millisecond, ns+"/"+name+" sleep "+reason, func() (bool, error) {
		var err error
		w, err = c.Get(context.Background(), ns, name)
		return err == nil && w.Sleep != nil && w.Sleep.Reason == reason, err
	})
	return w
}
