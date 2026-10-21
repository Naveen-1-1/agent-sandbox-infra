package workspace

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// maxRecheck bounds how long a running workspace goes without a fresh
// verdict, whatever the policy asked for.
const maxRecheck = time.Minute

// Verdict reasons the controller gives itself (policies give the rest).
const (
	reasonRequested     = "Requested"
	reasonModeOff       = "ModeOff"
	reasonAlwaysOn      = "AlwaysOn"
	reasonInvalidPolicy = "InvalidPolicy"
)

// sleepVerdict decides whether a running workspace should suspend now: an
// explicit request or mode Off always wins, AlwaysOn never suspends, and
// otherwise the workspace's policy decides on the merged signals. recheck
// is when to decide again (zero: only when something changes).
func (r *Reconciler) sleepVerdict(ctx context.Context, ws *sbxv1.Workspace) (v *sbxv1.SleepStatus, suspend bool, recheck time.Duration) {
	_, requested := ws.Annotations[sbxv1.AnnotationSuspendRequested]
	switch {
	case ws.Spec.Mode == sbxv1.ModeAlwaysOn:
		return &sbxv1.SleepStatus{Reason: reasonAlwaysOn, Message: "the workspace mode is AlwaysOn"}, false, 0
	case requested:
		return &sbxv1.SleepStatus{Reason: reasonRequested, Message: "a suspend was requested"}, true, 0
	case ws.Spec.Mode == sbxv1.ModeOff:
		return &sbxv1.SleepStatus{Reason: reasonModeOff, Message: "the workspace mode is Off"}, true, 0
	case r.Policies == nil:
		return ws.Status.Sleep, false, 0
	}
	pol, err := r.Policies.For(ctx, ws)
	if err != nil {
		// A policy we can't read must not put the workspace to sleep.
		return &sbxv1.SleepStatus{Reason: reasonInvalidPolicy, Message: err.Error()}, false, 0
	}
	view := policy.Workspace{Key: client.ObjectKeyFromObject(ws).String()}
	if ws.Status.PhaseSince != nil {
		view.AwakeSince = ws.Status.PhaseSince.Time
	}
	d := pol.Decide(view, r.signals(ctx, ws), r.Now())
	v = &sbxv1.SleepStatus{Policy: pol.Name(), Reason: string(d.Reason), Message: d.Message}
	if !d.SuspendAt.IsZero() {
		v.SuspendAt = &metav1.Time{Time: d.SuspendAt}
	}
	if d.RecheckAfter > 0 {
		recheck = min(max(d.RecheckAfter, time.Second), maxRecheck)
	}
	return v, d.Suspend, recheck
}

// signals merges what the agent, the gateways and status know about ws.
func (r *Reconciler) signals(ctx context.Context, ws *sbxv1.Workspace) policy.Signals {
	var s policy.Signals
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	if p, err := r.Pods.Agent(ws, r.pod(ws)).Signals(rctx); err != nil {
		s.Unknown = append(s.Unknown, "the agent")
	} else {
		s.Merge(policy.FromProto(p))
	}
	if r.Gateways != nil {
		if g, ok := r.Gateways.Signals(client.ObjectKeyFromObject(ws).String()); ok {
			s.Merge(g)
		} else {
			s.Unknown = append(s.Unknown, "the gateways")
		}
	}
	// Set when a wake finishes or a request aborts a suspend.
	if t := ws.Status.LastActivityAt; t != nil && t.After(s.LastActivity) {
		s.LastActivity = t.Time
	}
	return s
}

// sleepChanged reports whether a verdict differs enough from the recorded
// one to be worth a status write. The suspend time moves with every bit of
// activity, so small moves are ignored.
func sleepChanged(old, cur *sbxv1.SleepStatus) bool {
	if old == nil || cur == nil {
		return (old == nil) != (cur == nil)
	}
	if old.Policy != cur.Policy || old.Reason != cur.Reason || old.Message != cur.Message {
		return true
	}
	if (old.SuspendAt == nil) != (cur.SuspendAt == nil) {
		return true
	}
	if old.SuspendAt == nil {
		return false
	}
	d := old.SuspendAt.Sub(cur.SuspendAt.Time)
	return d > 5*time.Second || d < -5*time.Second
}

// countVerdict records a changed verdict.
func (r *Reconciler) countVerdict(v *sbxv1.SleepStatus, suspend bool) func() {
	return func() {
		if r.Sleep == nil || v == nil {
			return
		}
		decision := "keep"
		if suspend {
			decision = "suspend"
		}
		r.Sleep.Decisions.WithLabelValues(v.Policy, decision, v.Reason).Inc()
	}
}

// --- operation slots ---

func opKey(ws *sbxv1.Workspace) string { return client.ObjectKeyFromObject(ws).String() }

// acquire asks for a slot to start a wake or suspend.
func (r *Reconciler) acquire(ws *sbxv1.Workspace, c ops.Class) bool {
	return r.Ops == nil || r.Ops.Acquire(opKey(ws), c)
}

// hold keeps an operation in progress counted (after a restart, too).
func (r *Reconciler) hold(ws *sbxv1.Workspace, c ops.Class) {
	if r.Ops != nil {
		r.Ops.Hold(opKey(ws), c)
	}
}

// release ends ws's operation; a queued request for a new one stays queued.
func (r *Reconciler) release(ws *sbxv1.Workspace) {
	if r.Ops != nil {
		r.Ops.Release(opKey(ws))
	}
}

// forget drops ws from the operation queue: it no longer wants a slot.
func (r *Reconciler) forget(ws *sbxv1.Workspace) {
	if r.Ops != nil {
		r.Ops.Forget(opKey(ws))
	}
}

// wakeClass ranks a wake: one a request waits for goes first. A hinted
// wake moves up as soon as a gateway holds a request for it.
func (r *Reconciler) wakeClass(ws *sbxv1.Workspace, trigger sbxv1.WakeTrigger) ops.Class {
	if trigger != sbxv1.WakeTriggerHint {
		return ops.WakeRequest
	}
	if r.Gateways != nil {
		if s, ok := r.Gateways.Signals(opKey(ws)); ok && s.HeldRequests > 0 {
			return ops.WakeRequest
		}
	}
	return ops.WakeHint
}
