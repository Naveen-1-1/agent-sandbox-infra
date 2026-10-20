package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
)

// running watches a serving workspace: it notices a lost disk or pod
// changes, and starts a suspend when one is asked for.
func (r *Reconciler) running(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	if ws.Spec.Wake.Seq > ws.Status.ObservedWakeSeq { // already awake
		ws.Status.ObservedWakeSeq = ws.Spec.Wake.Seq
		return r.save(ctx, ws)
	}

	// A disk that vanished while awake: come back on a new disk from the
	// last snapshot rather than serve an empty one.
	if v := ws.Status.Volume; v != nil && v.EverBound {
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: v.ClaimName}, pvc)
		if apierrors.IsNotFound(err) {
			setCondition(ws, sbxv1.ConditionVolumeLost, metav1.ConditionTrue, "DiskMissing",
				fmt.Sprintf("disk %s disappeared; restoring the last snapshot into a new epoch", v.ClaimName))
			r.startWake(ws, sbxv1.WakeTriggerAPI)
			return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseWaking))
		} else if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Track pod restarts within the epoch (upstream recreates a lost pod).
	pod := &corev1.Pod{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: sandboxName(ws)}, pod); err == nil {
		ready := podCondition(pod, corev1.PodReady)
		if ws.Status.Pod == nil || ws.Status.Pod.UID != pod.UID || ws.Status.Pod.IP != pod.Status.PodIP {
			ws.Status.Pod = &sbxv1.PodStatus{Name: pod.Name, UID: pod.UID, IP: pod.Status.PodIP, NodeName: pod.Spec.NodeName}
			return r.save(ctx, ws)
		}
		if ready == nil || ready.Status != corev1.ConditionTrue {
			return reconcile.Result{RequeueAfter: 2 * time.Second}, nil // don't suspend a pod that isn't serving
		}
	} else {
		return reconcile.Result{RequeueAfter: 2 * time.Second}, client.IgnoreNotFound(err)
	}

	if wait, ok := r.suspendWanted(ws); ok {
		if wait > 0 {
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		now := metav1.Time{Time: r.Now()}
		ws.Status.Suspend = &sbxv1.SuspendStatus{
			AttemptID: newAttemptID(ws.Status.Epoch), PodUID: ws.Status.Pod.UID,
			StartedWakeSeq: ws.Spec.Wake.Seq, StartedAt: now, Step: sbxv1.SuspendStarted,
		}
		setCondition(ws, sbxv1.ConditionReady, metav1.ConditionFalse, "Suspending", "saving the workspace to S3")
		return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseSuspending))
	}
	return reconcile.Result{}, nil
}

// suspendWanted reports whether a suspend was asked for, and how long to
// wait first if a recent failure is backing it off.
func (r *Reconciler) suspendWanted(ws *sbxv1.Workspace) (time.Duration, bool) {
	if ws.Spec.Mode == sbxv1.ModeAlwaysOn {
		return 0, false
	}
	_, requested := ws.Annotations[sbxv1.AnnotationSuspendRequested]
	if !requested && ws.Spec.Mode != sbxv1.ModeOff {
		return 0, false
	}
	if t := ws.Status.SuspendRetryAt; t != nil && r.Now().Before(t.Time) {
		return t.Sub(r.Now()), true
	}
	return 0, true
}

// abortReason says why an uncommitted suspend must give way, or "".
func (r *Reconciler) abortReason(ctx context.Context, ws *sbxv1.Workspace) (string, error) {
	s := ws.Status.Suspend
	switch {
	case ws.Spec.Mode == sbxv1.ModeAlwaysOn:
		return "workspace mode is AlwaysOn", nil
	case ws.Spec.Wake.Seq != s.StartedWakeSeq && ws.Spec.Mode != sbxv1.ModeOff:
		return "a wake was requested", nil
	case r.Now().Sub(s.StartedAt.Time) > r.Config.SuspendTimeout:
		return "the suspend timed out", nil
	}
	// The snapshot is of one pod; if it restarted, its state may differ.
	if gone, err := r.podGone(ctx, ws, s.PodUID); err != nil {
		return "", err
	} else if gone {
		return "the pod restarted", nil
	}
	return "", nil
}

func (r *Reconciler) pod(ws *sbxv1.Workspace) Pod {
	return Pod{IP: ws.Status.Pod.IP, Epoch: ws.Status.Epoch}
}

// suspending freezes the workload, snapshots it and commits the snapshot.
// Until the commit, a wake or a failure returns the workspace to Running.
func (r *Reconciler) suspending(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	s := ws.Status.Suspend
	if s == nil {
		return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseRunning))
	}
	why, err := r.abortReason(ctx, ws)
	if err != nil {
		return reconcile.Result{}, err
	}
	if why != "" {
		return r.abort(ctx, ws, why)
	}
	agent := r.Pods.Agent(ws, r.pod(ws))
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()

	switch s.Step {
	case sbxv1.SuspendStarted:
		if err := agent.Freeze(rctx, s.AttemptID, r.Now().Add(r.Config.FreezeLease)); err != nil {
			return r.retryRPC(ctx, ws, "freezing the workload", err)
		}
		s.Step, s.FrozenAt = sbxv1.SuspendFrozen, &metav1.Time{Time: r.Now()}
		return r.save(ctx, ws)

	case sbxv1.SuspendFrozen:
		sidecar := r.Pods.Sidecar(ws, r.pod(ws))
		snap, err := sidecar.StartSnapshot(rctx, &sidecarv1.StartSnapshotRequest{
			AttemptId: s.AttemptID, Epoch: ws.Status.Epoch, BaseSha: ws.Status.Base.SHA,
			FreezeTime: timestamppb.New(s.StartedAt.Time), // fixed per attempt, so retries match
		})
		if err != nil {
			return r.retryRPC(ctx, ws, "starting the snapshot", err)
		}
		switch snap.GetState() {
		case sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING:
			// Keep the freeze alive while the snapshot runs.
			if err := agent.Freeze(rctx, s.AttemptID, r.Now().Add(r.Config.FreezeLease)); err != nil {
				return r.retryRPC(ctx, ws, "renewing the freeze", err)
			}
			return r.requeue()
		case sidecarv1.SnapshotState_SNAPSHOT_STATE_FAILED:
			return r.fail(ctx, ws, snap.GetError())
		}
		s.Snapshot = &sbxv1.SnapshotRef{
			Epoch: ws.Status.Epoch, WIPSHA: snap.GetWipSha(), BundleKey: snap.GetBundleKey(),
			SHA256: snap.GetSha256(), Bytes: snap.GetSizeBytes(),
		}
		s.Step = sbxv1.SuspendSnapshotted
		took := r.Now().Sub(s.StartedAt.Time)
		return r.save(ctx, ws, func() {
			if r.Snapshots != nil {
				r.Snapshots.Succeeded(snap.GetSizeBytes(), snap.GetNewBytes(), took)
			}
		})

	case sbxv1.SuspendSnapshotted, sbxv1.SuspendVerified:
		var prev int64
		if ws.Status.LastSnapshot != nil {
			prev = ws.Status.LastSnapshot.Seq
		}
		m, err := r.Store.Commit(ctx, prev, snapstore.Manifest{
			Workspace: string(ws.UID), Epoch: ws.Status.Epoch, AttemptID: s.AttemptID, WIPSHA: s.Snapshot.WIPSHA,
			BundleKey: s.Snapshot.BundleKey, SHA256: s.Snapshot.SHA256, Bytes: s.Snapshot.Bytes,
			Base: snapstore.Base{URL: ws.Status.Base.URL, SHA: ws.Status.Base.SHA, Branch: ws.Status.Base.Branch, Shallow: ws.Status.Base.Shallow},
		})
		switch {
		case errors.Is(err, snapstore.ErrBundleMismatch):
			return r.fail(ctx, ws, &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_UPLOAD_FAILED, Message: err.Error()})
		case errors.Is(err, snapstore.ErrStale):
			return r.fail(ctx, ws, &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_INTERNAL, Message: "a newer snapshot is already committed"})
		case err != nil:
			return r.retryRPC(ctx, ws, "committing the manifest", err)
		}
		created := metav1.Time{Time: m.CreatedAt}
		s.Snapshot.Seq, s.Snapshot.CreatedAt = m.Seq, &created
		ws.Status.LastSnapshot = s.Snapshot.DeepCopy() // a valid checkpoint even if we abort now
		s.Step = sbxv1.SuspendManifestWritten
		return r.save(ctx, ws)

	case sbxv1.SuspendManifestWritten:
		// Point of no return. Hold the freeze for good first; if the commit
		// below loses to a wake, the abort thaws it again.
		if err := agent.Freeze(rctx, s.AttemptID, time.Time{}); err != nil {
			return r.retryRPC(ctx, ws, "holding the freeze", err)
		}
		s.Committed, s.Step = true, sbxv1.SuspendCommitted
		setCondition(ws, sbxv1.ConditionSnapshotFailed, metav1.ConditionFalse, "Committed", "the last snapshot succeeded")
		ws.Status.SuspendFailures, ws.Status.SuspendRetryAt = 0, nil
		return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseTearingDown))
	}
	return reconcile.Result{}, fmt.Errorf("unexpected suspend step %q", s.Step)
}

// tearDown deletes the pod and the disk of a committed suspend. It never
// goes back: the snapshot is safe in S3.
func (r *Reconciler) tearDown(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	s := ws.Status.Suspend
	if s == nil || !s.Committed {
		return reconcile.Result{}, errors.New("tearing down without a committed suspend")
	}
	switch s.Step {
	case sbxv1.SuspendCommitted:
		sb := &sbv1.Sandbox{}
		if err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: sandboxName(ws)}, sb); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		} else if err == nil && sb.Spec.OperatingMode != sbv1.SandboxOperatingModeSuspended {
			return reconcile.Result{RequeueAfter: r.Config.Poll}, r.patchSandbox(ctx, sb, map[string]any{"operatingMode": sbv1.SandboxOperatingModeSuspended})
		}
		if gone, err := r.podGone(ctx, ws, ""); err != nil || !gone {
			return reconcile.Result{RequeueAfter: r.Config.Poll}, err
		}
		s.Step = sbxv1.SuspendPodGone
		return r.save(ctx, ws)

	case sbxv1.SuspendPodGone:
		gone, err := r.deleteClaim(ctx, ws, claimName(ws, ws.Status.Epoch))
		if err != nil || !gone {
			return reconcile.Result{RequeueAfter: r.Config.Poll}, err
		}
		s.Step = sbxv1.SuspendVolumeDeleted
		return r.save(ctx, ws)

	case sbxv1.SuspendVolumeDeleted:
		if err := r.clearSuspendRequest(ctx, ws); err != nil {
			return reconcile.Result{}, err
		}
		epoch := ws.Status.Epoch
		ws.Status.Suspend, ws.Status.Pod, ws.Status.Volume = nil, nil, nil
		setCondition(ws, sbxv1.ConditionReady, metav1.ConditionFalse, "Suspended", "saved to S3; wakes on the next request")
		setCondition(ws, sbxv1.ConditionFilesReady, metav1.ConditionFalse, "Suspended", "no disk while suspended")
		return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseSuspended), func() {
			r.deleteSecret(ctx, ws, epoch)
			if r.Metrics != nil {
				r.Metrics.Suspends.WithLabelValues(metrics.SuspendCommitted).Inc()
			}
		})
	}
	return reconcile.Result{}, fmt.Errorf("unexpected teardown step %q", s.Step)
}

// deleteClaim removes the snapshot-protection finalizer and deletes the
// disk. It reports true once the disk is gone or is being deleted with our
// finalizer off: what remains (Kubernetes' own pvc-protection) clears by
// itself once no pod uses the disk.
func (r *Reconciler) deleteClaim(ctx context.Context, ws *sbxv1.Workspace, name string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: name}, pvc)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if controllerutil.ContainsFinalizer(pvc, FinalizerSnapshotProtection) {
		patch := client.MergeFrom(pvc.DeepCopy())
		controllerutil.RemoveFinalizer(pvc, FinalizerSnapshotProtection)
		if err := r.Client.Patch(ctx, pvc, patch); err != nil {
			return false, client.IgnoreNotFound(err)
		}
	}
	if pvc.DeletionTimestamp.IsZero() {
		if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// suspended waits for a wake request.
func (r *Reconciler) suspended(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	if ws.Spec.Mode == sbxv1.ModeOff { // wake requests are ignored while Off
		if ws.Status.ObservedWakeSeq != ws.Spec.Wake.Seq {
			ws.Status.ObservedWakeSeq = ws.Spec.Wake.Seq
			return r.save(ctx, ws)
		}
		return reconcile.Result{}, nil
	}
	trigger := ws.Spec.Wake.Trigger
	if ws.Spec.Wake.Seq <= ws.Status.ObservedWakeSeq {
		if ws.Spec.Mode != sbxv1.ModeAlwaysOn {
			return reconcile.Result{}, nil
		}
		trigger = sbxv1.WakeTriggerAPI
	}
	if trigger == "" {
		trigger = sbxv1.WakeTriggerAPI
	}
	r.startWake(ws, trigger)
	return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseWaking), func() {
		if r.Metrics != nil {
			r.Metrics.Wakes.WithLabelValues(string(trigger)).Inc()
		}
	})
}

// startWake schedules the next epoch and makes it current, so the new pod's
// tokens are the ones the broker accepts. The resourceVersion-checked save
// that follows is what makes concurrent wake requests produce exactly one
// wake.
func (r *Reconciler) startWake(ws *sbxv1.Workspace, trigger sbxv1.WakeTrigger) {
	now := metav1.Time{Time: r.Now()}
	requested := now
	if ws.Spec.Wake.At != nil && ws.Spec.Wake.Seq > ws.Status.ObservedWakeSeq {
		requested = *ws.Spec.Wake.At
	}
	target := ws.Status.Epoch + 1
	ws.Status.Epoch = target
	var restore int64
	if ws.Status.LastSnapshot != nil {
		restore = ws.Status.LastSnapshot.Seq
	}
	ws.Status.ObservedWakeSeq = ws.Spec.Wake.Seq
	ws.Status.Wake = &sbxv1.WakeStatus{TargetEpoch: target, RestoreSeq: restore, Trigger: trigger, StartedAt: now}
	ws.Status.Volume = &sbxv1.VolumeStatus{ClaimName: claimName(ws, target)}
	ws.Status.Timeline = &sbxv1.WakeTimeline{Trigger: trigger, RequestedAt: &requested}
	setCondition(ws, sbxv1.ConditionReady, metav1.ConditionFalse, "Waking", fmt.Sprintf("restoring into epoch %d", target))
}

// abort returns an uncommitted suspend to Running and thaws the workload.
func (r *Reconciler) abort(ctx context.Context, ws *sbxv1.Workspace, why string) (reconcile.Result, error) {
	r.thaw(ctx, ws)
	logger(ctx, ws).Info("suspend aborted", "reason", why)
	if strings.Contains(why, "wake") {
		if err := r.clearSuspendRequest(ctx, ws); err != nil {
			return reconcile.Result{}, err
		}
	}
	ws.Status.Suspend = nil
	ws.Status.ObservedWakeSeq = ws.Spec.Wake.Seq
	ws.Status.LastActivityAt = &metav1.Time{Time: r.Now()}
	setCondition(ws, sbxv1.ConditionReady, metav1.ConditionTrue, "SuspendAborted", why)
	return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseRunning), func() {
		if r.Metrics != nil {
			r.Metrics.Suspends.WithLabelValues(metrics.SuspendAborted).Inc()
		}
	})
}

// fail records a failed snapshot, backs off, and keeps the workspace
// running: the pod and disk are never deleted without a committed snapshot.
func (r *Reconciler) fail(ctx context.Context, ws *sbxv1.Workspace, e *sidecarv1.SnapshotError) (reconcile.Result, error) {
	r.thaw(ctx, ws)
	reason, label := failureReason(e.GetCode())
	msg := e.GetMessage()
	if len(e.GetPaths()) > 0 {
		msg += ": " + strings.Join(e.GetPaths(), ", ")
	}
	ws.Status.SuspendFailures++
	backoff := min(30*time.Second<<min(ws.Status.SuspendFailures-1, 6), 30*time.Minute)
	ws.Status.SuspendRetryAt = &metav1.Time{Time: r.Now().Add(backoff)}
	ws.Status.Suspend = nil
	setCondition(ws, sbxv1.ConditionSnapshotFailed, metav1.ConditionTrue, reason, msg)
	setCondition(ws, sbxv1.ConditionReady, metav1.ConditionTrue, "SnapshotFailed", "still running; the snapshot will be retried")
	logger(ctx, ws).Info("snapshot failed; staying up", "reason", reason, "retryIn", backoff.String())
	return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseRunning), func() {
		if r.Snapshots != nil {
			r.Snapshots.Failed(label)
		}
		if r.Metrics != nil {
			r.Metrics.Suspends.WithLabelValues(metrics.SuspendFailed).Inc()
		}
	})
}

func failureReason(c sidecarv1.SnapshotErrorCode) (reason, label string) {
	switch c {
	case sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_SECRET_DETECTED:
		return "SecretDetected", metrics.ReasonSecretDetected
	case sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_TOO_LARGE:
		return "TooLarge", metrics.ReasonTooLarge
	case sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_UPLOAD_FAILED:
		return "UploadFailed", metrics.ReasonUploadFailed
	case sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_GIT_FAILED:
		return "GitFailed", metrics.ReasonGitFailed
	}
	return "Internal", metrics.ReasonInternal
}

// retryRPC retries a failed pod call shortly; abortReason gives up once the
// suspend times out. Nothing is written, so a retry can't spin on its own
// status updates.
func (r *Reconciler) retryRPC(ctx context.Context, ws *sbxv1.Workspace, what string, err error) (reconcile.Result, error) {
	logger(ctx, ws).Info("pod call failed; retrying", "step", what, "err", err.Error())
	return reconcile.Result{RequeueAfter: time.Second}, nil
}

func (r *Reconciler) thaw(ctx context.Context, ws *sbxv1.Workspace) {
	if ws.Status.Suspend == nil || ws.Status.Pod == nil {
		return
	}
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	if err := r.Pods.Agent(ws, r.pod(ws)).Thaw(rctx, ws.Status.Suspend.AttemptID); err != nil {
		logger(ctx, ws).Info("thaw failed; the freeze lease will expire", "err", err.Error())
	}
}

func (r *Reconciler) clearSuspendRequest(ctx context.Context, ws *sbxv1.Workspace) error {
	if _, ok := ws.Annotations[sbxv1.AnnotationSuspendRequested]; !ok {
		return nil
	}
	patch := []byte(`{"metadata":{"annotations":{"` + sbxv1.AnnotationSuspendRequested + `":null}}}`)
	obj := &sbxv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: ws.Name}}
	if err := r.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return client.IgnoreNotFound(err)
	}
	delete(ws.Annotations, sbxv1.AnnotationSuspendRequested)
	ws.ResourceVersion = obj.ResourceVersion
	return nil
}
