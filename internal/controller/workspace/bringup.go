package workspace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// startProvisioning pins the base commit and schedules epoch 1. First
// provisioning is a wake with nothing to restore but the base.
func (r *Reconciler) startProvisioning(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	base, err := r.Inspector.Resolve(ctx, ws.Spec.Source)
	var perm PermanentError
	if errors.As(err, &perm) {
		setCondition(ws, sbxv1.ConditionReady, metav1.ConditionFalse, "SourceInvalid", err.Error())
		return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseFailed))
	}
	if err != nil {
		logger(ctx, ws).Info("resolving source failed; retrying", "err", err.Error())
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !r.acquire(ws, ops.WakeRequest) {
		return r.requeue()
	}
	now := metav1.Time{Time: r.Now()}
	image := ws.Spec.Image
	if image == "" {
		image = r.Config.DefaultImage
	}
	ws.Status.Base = &sbxv1.BaseStatus{
		URL: snapshot.StripCredentials(ws.Spec.Source.RepoURL), SHA: base.SHA, Branch: base.Branch,
		Shallow: ws.Spec.Source.Depth > 0,
	}
	ws.Status.ResolvedImage = image
	ws.Status.Epoch = 1 // the broker accepts only the current epoch's tokens
	ws.Status.ObservedWakeSeq = ws.Spec.Wake.Seq
	ws.Status.Wake = &sbxv1.WakeStatus{TargetEpoch: 1, Trigger: sbxv1.WakeTriggerAPI, StartedAt: now}
	ws.Status.Volume = &sbxv1.VolumeStatus{ClaimName: claimName(ws, 1)}
	ws.Status.Timeline = &sbxv1.WakeTimeline{Trigger: sbxv1.WakeTriggerAPI, RequestedAt: &metav1.MicroTime{Time: now.Time}}
	resetDeps(ws)
	setCondition(ws, sbxv1.ConditionReady, metav1.ConditionFalse, "Provisioning", "creating the first pod and disk")
	return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseProvisioning))
}

// bringUp starts the epoch named by status.wake and waits for its pod.
func (r *Reconciler) bringUp(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	if ws.Status.Wake == nil {
		return reconcile.Result{}, errors.New("waking without status.wake")
	}
	target := ws.Status.Wake.TargetEpoch

	sb := &sbv1.Sandbox{}
	err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: sandboxName(ws)}, sb)
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	exists := err == nil

	// 1. No other epoch's pod may run alongside this one.
	if exists && templateEpoch(sb) != target {
		if sb.Spec.OperatingMode != sbv1.SandboxOperatingModeSuspended {
			return reconcile.Result{RequeueAfter: r.Config.Poll}, r.patchSandbox(ctx, sb, map[string]any{"operatingMode": sbv1.SandboxOperatingModeSuspended})
		}
		if gone, err := r.podGone(ctx, ws, ""); err != nil || !gone {
			return reconcile.Result{RequeueAfter: r.Config.Poll}, err
		}
	}

	// 2. This epoch's tokens and disk.
	if err := r.ensureSecret(ctx, ws, target); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureClaim(ctx, ws, target); err != nil {
		return reconcile.Result{}, err
	}

	// 3. The pod template and Running, in one write.
	tmpl, err := r.template(ws, target)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !exists {
		sb = &sbv1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: sandboxName(ws), Labels: map[string]string{podspec.LabelWorkspace: ws.Name}},
			Spec: sbv1.SandboxSpec{
				SandboxBlueprint: sbv1.SandboxBlueprint{PodTemplate: tmpl, Service: ptr.To(false)},
				OperatingMode:    sbv1.SandboxOperatingModeRunning,
			},
		}
		if err := controllerutil.SetControllerReference(ws, sb, r.Scheme); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.Client.Create(ctx, sb); err != nil && !apierrors.IsAlreadyExists(err) {
			return reconcile.Result{}, err
		}
		return r.requeue()
	}
	if templateEpoch(sb) != target || sb.Spec.OperatingMode != sbv1.SandboxOperatingModeRunning {
		if err := r.patchSandbox(ctx, sb, map[string]any{"operatingMode": sbv1.SandboxOperatingModeRunning, "podTemplate": tmpl}); err != nil {
			return reconcile.Result{}, err
		}
		return r.requeue()
	}

	// 4. Wait for this epoch's pod to be ready.
	pod, err := r.readyPod(ctx, ws, sb, target)
	if err != nil || pod == nil {
		return reconcile.Result{RequeueAfter: r.Config.Poll}, err
	}

	// 5. Record the wake and go Running.
	now := r.Now()
	tl := ws.Status.Timeline
	if tl == nil {
		tl = &sbxv1.WakeTimeline{}
		ws.Status.Timeline = tl
	}
	if c := podCondition(pod, corev1.PodScheduled); c != nil {
		tl.PodScheduledAt = &metav1.MicroTime{Time: c.LastTransitionTime.Time}
	}
	filesAt, restored := r.filesReady(ctx, ws, pod, target)
	if !filesAt.IsZero() {
		tl.FilesReadyAt = &metav1.MicroTime{Time: filesAt}
	}
	tl.ReadyAt = &metav1.MicroTime{Time: now}

	trigger := ws.Status.Wake.Trigger
	ws.Status.Pod = &sbxv1.PodStatus{Name: pod.Name, UID: pod.UID, IP: pod.Status.PodIP, NodeName: pod.Spec.NodeName}
	ws.Status.Volume = &sbxv1.VolumeStatus{ClaimName: claimName(ws, target), EverBound: true}
	ws.Status.Wake = nil
	ws.Status.LastActivityAt = &metav1.Time{Time: now}
	setCondition(ws, sbxv1.ConditionReady, metav1.ConditionTrue, "PodReady", fmt.Sprintf("epoch %d is serving", target))
	setCondition(ws, sbxv1.ConditionFilesReady, metav1.ConditionTrue, "Restored", "workspace files are in place")
	meta.RemoveStatusCondition(&ws.Status.Conditions, sbxv1.ConditionVolumeLost)
	return r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseRunning), func() {
		r.observeWake(tl, string(trigger), restored)
		r.deleteSecret(ctx, ws, target-1)
	})
}

func (r *Reconciler) observeWake(tl *sbxv1.WakeTimeline, trigger string, restored int64) {
	if r.Metrics == nil || tl == nil || tl.RequestedAt == nil {
		return
	}
	if tl.FilesReadyAt != nil {
		r.Metrics.WakeDuration.WithLabelValues(metrics.StageFilesReady, trigger).Observe(tl.FilesReadyAt.Sub(tl.RequestedAt.Time).Seconds())
	}
	if tl.ReadyAt != nil {
		r.Metrics.WakeDuration.WithLabelValues(metrics.StageReady, trigger).Observe(tl.ReadyAt.Sub(tl.RequestedAt.Time).Seconds())
	}
	if restored > 0 {
		r.Metrics.RestoreBytes.Observe(float64(restored))
	}
}

// filesReady asks the sidecar when the files landed; failing that, it uses
// the restore init container's finish time.
func (r *Reconciler) filesReady(ctx context.Context, ws *sbxv1.Workspace, pod *corev1.Pod, epoch int64) (time.Time, int64) {
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	if at, n, err := r.Pods.Sidecar(ws, Pod{IP: pod.Status.PodIP, Epoch: epoch}).FilesReady(rctx); err == nil {
		return at, n
	}
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name == podspec.ContainerRestore && s.State.Terminated != nil {
			return s.State.Terminated.FinishedAt.Time, 0
		}
	}
	return time.Time{}, 0
}

// readyPod returns the Sandbox's pod once it serves epoch and is ready.
func (r *Reconciler) readyPod(ctx context.Context, ws *sbxv1.Workspace, sb *sbv1.Sandbox, epoch int64) (*corev1.Pod, error) {
	if !meta.IsStatusConditionTrue(sb.Status.Conditions, string(sbv1.SandboxConditionReady)) {
		return nil, nil
	}
	pod := &corev1.Pod{}
	if err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: sandboxName(ws)}, pod); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if pod.Labels[podspec.LabelEpoch] != strconv.FormatInt(epoch, 10) || !pod.DeletionTimestamp.IsZero() || pod.Status.PodIP == "" {
		return nil, nil
	}
	if c := podCondition(pod, corev1.PodReady); c == nil || c.Status != corev1.ConditionTrue {
		return nil, nil
	}
	return pod, nil
}

// podGone reports whether the workspace's pod no longer exists (or, if uid
// is set, whether that particular pod is gone).
func (r *Reconciler) podGone(ctx context.Context, ws *sbxv1.Workspace, uid types.UID) (bool, error) {
	pod := &corev1.Pod{}
	err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: sandboxName(ws)}, pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return uid != "" && pod.UID != uid, nil
}

func podCondition(pod *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == t {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func templateEpoch(sb *sbv1.Sandbox) int64 {
	n, _ := strconv.ParseInt(sb.Spec.PodTemplate.ObjectMeta.Labels[podspec.LabelEpoch], 10, 64)
	return n
}

func (r *Reconciler) template(ws *sbxv1.Workspace, epoch int64) (sbv1.PodTemplate, error) {
	c := r.Config
	return podspec.Build(podspec.Input{
		Workspace: ws, Epoch: epoch, ClaimName: claimName(ws, epoch),
		Image: ws.Status.ResolvedImage, RuntimeImage: c.RuntimeImage, PullPolicy: c.PullPolicy,
		ImagePullSecrets: c.ImagePullSecrets, TokenSecret: secretName(ws, epoch),
		AgentTokenSHA256: hex.EncodeToString(token.SHA256(r.Keys.Derive(token.Agent, string(ws.UID), epoch))),
		BrokerAddr:       c.BrokerAddr, Resources: c.Resources, RunAsUser: c.RunAsUser, RunAsGroup: c.RunAsGroup,
		Snapshot: c.Snapshot,
	})
}

// patchSandbox merge-patches spec fields with an optimistic lock.
func (r *Reconciler) patchSandbox(ctx context.Context, sb *sbv1.Sandbox, spec map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": sb.ResourceVersion},
		"spec":     spec,
	})
	if err != nil {
		return err
	}
	return ignoreConflict(r.Client.Patch(ctx, sb, client.RawPatch(types.MergePatchType, body)))
}

func (r *Reconciler) labels(ws *sbxv1.Workspace, epoch int64) map[string]string {
	return map[string]string{podspec.LabelWorkspace: ws.Name, podspec.LabelEpoch: strconv.FormatInt(epoch, 10)}
}

// ensureSecret creates the epoch's token Secret if it is missing.
func (r *Reconciler) ensureSecret(ctx context.Context, ws *sbxv1.Workspace, epoch int64) error {
	err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: secretName(ws, epoch)}, &corev1.Secret{})
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: secretName(ws, epoch), Labels: r.labels(ws, epoch)},
		Type:       corev1.SecretTypeOpaque,
		StringData: podspec.Tokens(r.Keys, string(ws.UID), epoch),
	}
	if err := controllerutil.SetControllerReference(ws, s, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *Reconciler) deleteSecret(ctx context.Context, ws *sbxv1.Workspace, epoch int64) {
	if epoch < 1 {
		return
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: secretName(ws, epoch)}}
	if err := r.Client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		logger(ctx, ws).Error(err, "deleting old token secret", "epoch", epoch)
	}
}

// ensureClaim creates the epoch's disk if it has never existed. A disk that
// was bound once and is now missing is not recreated: an empty replacement
// would look like lost work.
func (r *Reconciler) ensureClaim(ctx context.Context, ws *sbxv1.Workspace, epoch int64) error {
	name := claimName(ws, epoch)
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.API.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: name}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if v := ws.Status.Volume; v != nil && v.ClaimName == name && v.EverBound {
		return fmt.Errorf("disk %s was bound and has disappeared", name)
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ws.Namespace, Name: name, Labels: r.labels(ws, epoch),
			Finalizers: []string{FinalizerSnapshotProtection},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: r.Config.StorageSize}},
		},
	}
	if r.Config.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptr.To(r.Config.StorageClassName)
	}
	if err := controllerutil.SetControllerReference(ws, pvc, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
