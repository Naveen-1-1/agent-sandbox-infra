package workspace_test

import (
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
)

func TestProvisionSuspendWake(t *testing.T) {
	ns := namespace(t, "flow-")
	ws := newWorkspace(t, ns, "demo")

	cur := waitFor(t, ws, "provisioned", phase(sbxv1.PhaseRunning, 1))
	if cur.Status.Base.SHA != baseSHA || cur.Status.ResolvedImage != "image:test" || !cur.Status.Volume.EverBound {
		t.Fatalf("status after provisioning = %+v", cur.Status)
	}
	if !claimExists(t, ns, "ws-demo-e1") {
		t.Fatal("epoch 1 disk missing")
	}
	sb := sandbox(t, ws)
	if sb.Spec.PodTemplate.ObjectMeta.Labels[podspec.LabelEpoch] != "1" || !metav1IsControlledBy(sb, cur) {
		t.Fatalf("sandbox template/owner wrong: %+v", sb.ObjectMeta)
	}

	// Suspend: snapshot committed, then pod and disk gone.
	requestSuspend(t, ws)
	cur = waitFor(t, ws, "suspended", phase(sbxv1.PhaseSuspended, 1))
	if claimExists(t, ns, "ws-demo-e1") {
		t.Error("epoch 1 disk still exists after suspend")
	}
	if sandbox(t, ws).Spec.OperatingMode != sbv1.SandboxOperatingModeSuspended {
		t.Error("sandbox not suspended")
	}
	if _, ok := cur.Annotations[sbxv1.AnnotationSuspendRequested]; ok {
		t.Error("suspend request annotation not cleared")
	}
	m, ok, err := snapstore.New(store).Latest(context.Background(), string(cur.UID))
	if err != nil || !ok || m.Seq != 1 || cur.Status.LastSnapshot == nil || cur.Status.LastSnapshot.BundleKey != m.BundleKey {
		t.Fatalf("manifest %+v (ok=%v, err=%v) vs lastSnapshot %+v", m, ok, err, cur.Status.LastSnapshot)
	}

	// Wake: a new epoch on a new disk, restoring snapshot 1.
	requestWake(t, ws, sbxv1.WakeTriggerHint)
	cur = waitFor(t, ws, "woken", phase(sbxv1.PhaseRunning, 2))
	if !claimExists(t, ns, "ws-demo-e2") || sandbox(t, ws).Spec.PodTemplate.ObjectMeta.Labels[podspec.LabelEpoch] != "2" {
		t.Fatal("epoch 2 disk or template missing")
	}
	tl := cur.Status.Timeline
	if tl == nil || tl.Trigger != sbxv1.WakeTriggerHint || tl.RequestedAt == nil || tl.FilesReadyAt == nil || tl.ReadyAt == nil {
		t.Fatalf("wake timeline = %+v", tl)
	}
	if cur.Status.ObservedWakeSeq != cur.Spec.Wake.Seq {
		t.Errorf("observedWakeSeq %d != spec %d", cur.Status.ObservedWakeSeq, cur.Spec.Wake.Seq)
	}

	// A second cycle builds on the first snapshot.
	requestSuspend(t, ws)
	cur = waitFor(t, ws, "suspended again", phase(sbxv1.PhaseSuspended, 2))
	if cur.Status.LastSnapshot.Seq != 2 {
		t.Errorf("second snapshot seq = %d, want 2", cur.Status.LastSnapshot.Seq)
	}
}

func TestWakeDuringSuspendAbortsIt(t *testing.T) {
	ns := namespace(t, "abort-")
	ws := newWorkspace(t, ns, "demo")
	cur := waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))
	pods.block(cur, true)
	requestSuspend(t, ws)
	waitFor(t, ws, "snapshot in progress", func(w *sbxv1.Workspace) bool {
		return w.Status.Phase == sbxv1.PhaseSuspending && w.Status.Suspend != nil && w.Status.Suspend.Step == sbxv1.SuspendFrozen
	})
	ip := cur.Status.Pod.IP
	if frozen, _ := pods.isFrozen(ip); !frozen {
		t.Fatal("workload not frozen during the snapshot")
	}

	requestWake(t, ws, sbxv1.WakeTriggerRequest)
	cur = waitFor(t, ws, "suspend aborted", phase(sbxv1.PhaseRunning, 1))
	pods.block(cur, false)
	if frozen, _ := pods.isFrozen(ip); frozen {
		t.Error("workload still frozen after the abort")
	}
	if !claimExists(t, ns, "ws-demo-e1") || cur.Status.Suspend != nil {
		t.Error("abort lost the disk or left a suspend behind")
	}
	if _, ok := cur.Annotations[sbxv1.AnnotationSuspendRequested]; ok {
		t.Error("the wake should have withdrawn the suspend request")
	}
}

func TestSnapshotFailureKeepsTheWorkspaceRunning(t *testing.T) {
	ns := namespace(t, "fail-")
	ws := newWorkspace(t, ns, "demo")
	cur := waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))
	pods.failSnapshots(cur, sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_SECRET_DETECTED)
	before := testutil.ToFloat64(wsm.Suspends.WithLabelValues(metrics.SuspendFailed))

	requestSuspend(t, ws)
	cur = waitFor(t, ws, "snapshot failure recorded", func(w *sbxv1.Workspace) bool {
		return w.Status.Phase == sbxv1.PhaseRunning && meta.IsStatusConditionTrue(w.Status.Conditions, sbxv1.ConditionSnapshotFailed)
	})
	c := meta.FindStatusCondition(cur.Status.Conditions, sbxv1.ConditionSnapshotFailed)
	if c.Reason != "SecretDetected" || cur.Status.SuspendRetryAt == nil || cur.Status.SuspendFailures != 1 {
		t.Errorf("condition %+v, retryAt %v, failures %d", c, cur.Status.SuspendRetryAt, cur.Status.SuspendFailures)
	}
	if !claimExists(t, ns, "ws-demo-e1") {
		t.Fatal("the disk was deleted although the snapshot failed")
	}
	if frozen, _ := pods.isFrozen(cur.Status.Pod.IP); frozen {
		t.Error("workload left frozen after a failed snapshot")
	}
	if got := testutil.ToFloat64(wsm.Suspends.WithLabelValues(metrics.SuspendFailed)); got != before+1 {
		t.Errorf("failed suspends metric = %v, want %v", got, before+1)
	}
}

func TestConcurrentWakeRequestsWakeOnce(t *testing.T) {
	ns := namespace(t, "storm-")
	ws := newWorkspace(t, ns, "demo")
	waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))
	requestSuspend(t, ws)
	waitFor(t, ws, "suspended", phase(sbxv1.PhaseSuspended, 1))
	before := kubelet.Started()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestWake(t, ws, sbxv1.WakeTriggerRequest)
		}()
	}
	wg.Wait()
	cur := waitFor(t, ws, "woken", phase(sbxv1.PhaseRunning, 2))
	if cur.Status.Epoch != 2 {
		t.Fatalf("epoch = %d after 20 concurrent wake requests, want exactly one wake (2)", cur.Status.Epoch)
	}
	if started := kubelet.Started() - before; started != 1 {
		t.Errorf("%d pods started for one wake, want 1", started)
	}
}

func TestDeletePurgesEverything(t *testing.T) {
	ns := namespace(t, "delete-")
	ws := newWorkspace(t, ns, "demo")
	waitFor(t, ws, "running", phase(sbxv1.PhaseRunning, 1))
	requestSuspend(t, ws)
	cur := waitFor(t, ws, "suspended", phase(sbxv1.PhaseSuspended, 1))
	requestWake(t, ws, sbxv1.WakeTriggerAPI)
	waitFor(t, ws, "woken", phase(sbxv1.PhaseRunning, 2))
	uid := string(cur.UID)

	if err := k8s.Delete(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	waitGone(t, cur)
	if objs, _ := store.List(context.Background(), "ws/"+uid+"/"); len(objs) != 0 {
		t.Errorf("%d objects left in S3 for the deleted workspace", len(objs))
	}
	if claimExists(t, ns, "ws-demo-e2") {
		t.Error("disk left behind")
	}
}

func waitGone(t *testing.T, ws *sbxv1.Workspace) {
	t.Helper()
	for range 1500 {
		err := k8s.Get(context.Background(), client.ObjectKeyFromObject(ws), &sbxv1.Workspace{})
		if client.IgnoreNotFound(err) == nil && err != nil {
			return
		}
		pause()
	}
	t.Fatal("workspace was not deleted")
}

func metav1IsControlledBy(obj client.Object, owner client.Object) bool {
	for _, r := range obj.GetOwnerReferences() {
		if r.UID == owner.GetUID() && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func TestPausedWorkspaceIsLeftAlone(t *testing.T) {
	ns := namespace(t, "paused-")
	ws := &sbxv1.Workspace{}
	ws.Namespace, ws.Name = ns, "demo"
	ws.Annotations = map[string]string{sbxv1.AnnotationPaused: "true"}
	ws.Spec.Source.RepoURL = "https://example.com/r.git"
	if err := k8s.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	for range 25 {
		pause()
	}
	if cur := get(t, ws); cur.Status.Phase != "" || len(cur.Finalizers) != 0 {
		t.Fatalf("paused workspace was reconciled: phase %q finalizers %v", cur.Status.Phase, cur.Finalizers)
	}
}
