package workspace_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// withPolicy creates a running workspace with its own sleep policy and an
// agent that was last used an hour ago.
func withPolicy(t *testing.T, prefix string, ref *sbxv1.PolicyRef) *sbxv1.Workspace {
	t.Helper()
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace(t, prefix), Name: "demo"},
		Spec:       sbxv1.WorkspaceSpec{Source: sbxv1.Source{RepoURL: "https://example.com/r.git"}, Policy: ref},
	}
	if err := k8s.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	pods.setSignals(ws, idleAgent())
	return ws
}

func idleAgent() *signalsv1.Signals {
	return &signalsv1.Signals{LastActivity: timestamppb.New(time.Now().Add(-time.Hour))}
}

func fixed(params ...string) *sbxv1.PolicyRef {
	ref := &sbxv1.PolicyRef{Name: "fixed", Params: map[string]string{}}
	for i := 0; i+1 < len(params); i += 2 {
		ref.Params[params[i]] = params[i+1]
	}
	return ref
}

func sleepReason(reason string) func(*sbxv1.Workspace) bool {
	return func(ws *sbxv1.Workspace) bool {
		return ws.Status.Phase == sbxv1.PhaseRunning && ws.Status.Sleep != nil && ws.Status.Sleep.Reason == reason
	}
}

// poke makes the controller look at ws now, as a change in its signals would.
func poke(t *testing.T, ws *sbxv1.Workspace) {
	t.Helper()
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{"test/poke":%q}}}`, time.Now().Format(time.RFC3339Nano))
	if err := k8s.Patch(context.Background(), ws, client.RawPatch("application/merge-patch+json", patch)); err != nil {
		t.Fatal(err)
	}
}

func TestIdleWorkspaceSuspendsItself(t *testing.T) {
	ws := withPolicy(t, "idle-", fixed("idleTimeout", "5s"))
	cur := waitFor(t, ws, "the idle timer running", sleepReason(string(policy.ReasonActive)))
	if at := cur.Status.Sleep.SuspendAt; at == nil || at.Sub(cur.Status.PhaseSince.Time) > 6*time.Second {
		t.Fatalf("suspendAt = %v, want 5s after it started serving (%v)", at, cur.Status.PhaseSince)
	}
	before := testutil.ToFloat64(sleepm.Decisions.WithLabelValues("fixed", "suspend", "Idle"))

	cur = waitFor(t, ws, "suspended by the policy", phase(sbxv1.PhaseSuspended, 1))
	if s := cur.Status.Sleep; s == nil || s.Policy != "fixed" || s.Reason != string(policy.ReasonIdle) {
		t.Fatalf("sleep status after suspending = %+v, want fixed/Idle", s)
	}
	if testutil.ToFloat64(sleepm.Decisions.WithLabelValues("fixed", "suspend", "Idle")) != before+1 {
		t.Error("the suspend verdict wasn't counted once")
	}

	requestWake(t, ws, sbxv1.WakeTriggerRequest)
	cur = waitFor(t, ws, "awake again", phase(sbxv1.PhaseRunning, 2))
	if s := cur.Status.Sleep; s != nil && s.Reason == string(policy.ReasonIdle) {
		t.Error("the old suspend verdict survived the wake")
	}
}

func TestVetoesKeepAWorkspaceAwake(t *testing.T) {
	ws := withPolicy(t, "veto-", fixed("idleTimeout", "5s"))
	waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))

	pods.setSignals(ws, &signalsv1.Signals{
		LastActivity:    timestamppb.New(time.Now().Add(-time.Hour)),
		RunningCommands: []*signalsv1.Command{{Command: "npm test", Kind: signalsv1.CommandKind_COMMAND_KIND_EXEC}},
	})
	poke(t, ws)
	waitFor(t, ws, "kept up by a running command", sleepReason(string(policy.ReasonCommandRunning)))

	pods.setSignals(ws, nil) // the agent stops answering
	poke(t, ws)
	waitFor(t, ws, "kept up while the agent can't be read", sleepReason(string(policy.ReasonSignalsUnknown)))

	pods.setSignals(ws, idleAgent())
	gateways.set(ws, policy.Signals{HeldRequests: 3})
	poke(t, ws)
	waitFor(t, ws, "kept up by held requests", sleepReason(string(policy.ReasonRequestsHeld)))

	time.Sleep(6 * time.Second) // well past the idle timeout
	if cur := get(t, ws); cur.Status.Phase != sbxv1.PhaseRunning {
		t.Fatalf("phase %s while vetoed", cur.Status.Phase)
	}

	gateways.set(ws, policy.Signals{})
	poke(t, ws)
	waitFor(t, ws, "suspended once nothing holds it", phase(sbxv1.PhaseSuspended, 1))
}

func TestTurnEndedSuspendsSooner(t *testing.T) {
	ws := withPolicy(t, "turn-", fixed("idleTimeout", "1h", "afterTurnEnded", "1s"))
	waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))
	pods.setSignals(ws, &signalsv1.Signals{LastActivity: timestamppb.New(time.Now())})
	poke(t, ws)
	cur := waitFor(t, ws, "the long idle timer", sleepReason(string(policy.ReasonActive)))
	if at := cur.Status.Sleep.SuspendAt; at == nil || time.Until(at.Time) < 50*time.Minute {
		t.Fatalf("suspendAt = %v before the turn ended, want about an hour away", at)
	}

	gateways.set(ws, policy.Signals{LastTurnEnded: time.Now().Add(time.Second)})
	poke(t, ws)
	cur = waitFor(t, ws, "suspended after the turn ended", phase(sbxv1.PhaseSuspended, 1))
	if cur.Status.Sleep.Reason != string(policy.ReasonTurnEnded) {
		t.Fatalf("reason = %s, want TurnEnded", cur.Status.Sleep.Reason)
	}
}

func TestInvalidWorkspacePolicyNeverSuspends(t *testing.T) {
	ws := withPolicy(t, "badpolicy-", &sbxv1.PolicyRef{Name: "sometimes"})
	cur := waitFor(t, ws, "flagged", sleepReason("InvalidPolicy"))
	if cur.Status.Sleep.Message == "" {
		t.Error("no explanation for the invalid policy")
	}
}

func TestClusterPolicyHotSwap(t *testing.T) {
	ctx := context.Background()
	ws := withPolicy(t, "hotswap-", nil)
	waitFor(t, ws, "kept by the default (never)", sleepReason(string(policy.ReasonNever)))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: policyNS, Name: sleep.PolicyConfigMap},
		Data:       map[string]string{policy.ConfigKey: "name: fixed\nparams:\n  idleTimeout: 5s\n"},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), cm) })
	waitFor(t, ws, "suspended under the new policy", phase(sbxv1.PhaseSuspended, 1))

	// A broken edit is reported and the previous policy stays in force: the
	// workspace still goes back to sleep after waking.
	cm.Data[policy.ConfigKey] = "name: fixed\nparams:\n  idleTimeout: whenever\n"
	if err := k8s.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	requestWake(t, ws, sbxv1.WakeTriggerRequest)
	waitFor(t, ws, "awake", phase(sbxv1.PhaseRunning, 2))
	cur := waitFor(t, ws, "suspended under the previous policy", phase(sbxv1.PhaseSuspended, 2))
	if cur.Status.Sleep.Policy != "fixed" {
		t.Fatalf("suspended by %q, want fixed", cur.Status.Sleep.Policy)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		evs := &eventsv1.EventList{}
		if err := k8s.List(ctx, evs, client.InNamespace(policyNS)); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range evs.Items {
			found = found || (e.Regarding.Name == sleep.PolicyConfigMap && e.Reason == "InvalidPolicy" && e.Type == corev1.EventTypeWarning)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no InvalidPolicy warning on the ConfigMap")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
