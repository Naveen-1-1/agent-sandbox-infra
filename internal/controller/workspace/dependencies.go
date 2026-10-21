package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
)

// depsPoll is how often a pending dependency restore is checked.
const depsPoll = time.Second

// resetDeps marks a new epoch's dependencies as not restored yet, so
// commands that need them wait from the first moment the pod serves.
func resetDeps(ws *sbxv1.Workspace) {
	ws.Status.Deps = nil
	setCondition(ws, sbxv1.ConditionDepsReady, metav1.ConditionFalse, sbxv1.DepsReasonPending, "waiting for the workspace files")
}

// workloadImageID is the digest the workload container runs, once known.
func workloadImageID(pod *corev1.Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == podspec.ContainerWorkload && c.State.Running != nil {
			return c.ImageID
		}
	}
	return ""
}

// kickDeps starts the dependency restore as soon as the workload's image
// is known, without waiting for the pod to be ready. Best effort:
// trackDeps starts it anyway once the workspace runs.
func (r *Reconciler) kickDeps(ctx context.Context, ws *sbxv1.Workspace, pod *corev1.Pod, epoch int64) {
	image := workloadImageID(pod)
	if image == "" || pod.Status.PodIP == "" || !sbxv1.DepsPending(ws) {
		return
	}
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	if _, err := r.Pods.Sidecar(ws, Pod{IP: pod.Status.PodIP, Epoch: epoch}).StartDeps(rctx, image); err != nil {
		logger(ctx, ws).V(1).Info("starting the dependency restore early failed", "err", err.Error())
	}
}

// trackDeps follows this epoch's dependency restore until it settles. It
// reports whether status changed (with hooks to run once it is saved) and
// whether the restore is still pending.
func (r *Reconciler) trackDeps(ctx context.Context, ws *sbxv1.Workspace, pod *corev1.Pod) (changed bool, after func(), pending bool) {
	if !sbxv1.DepsPending(ws) {
		return false, nil, false
	}
	image := workloadImageID(pod)
	if image == "" {
		return false, nil, true
	}
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	d, err := r.Pods.Sidecar(ws, r.pod(ws)).StartDeps(rctx, image)
	if err != nil {
		logger(ctx, ws).Info("checking the dependency restore failed; retrying", "err", err.Error())
		return false, nil, true
	}

	status, reason, msg := depsCondition(d)
	cur := &sbxv1.DepsStatus{Profile: d.GetProfile(), Key: d.GetKey(), Source: depsSource(d.GetSource()), Bytes: d.GetBytes()}
	old := meta.FindStatusCondition(ws.Status.Conditions, sbxv1.ConditionDepsReady)
	if old != nil && old.Reason == reason && old.Message == msg && equalDeps(ws.Status.Deps, cur) {
		return false, nil, true
	}
	ws.Status.Deps = cur
	setCondition(ws, sbxv1.ConditionDepsReady, status, reason, msg)
	pending = sbxv1.DepsPending(ws)
	if pending {
		return true, nil, true
	}

	// Settled: record when, and how it went.
	readyAt := r.Now()
	if t := d.GetFinishedAt(); t != nil {
		readyAt = t.AsTime()
	}
	tl := ws.Status.Timeline
	if tl == nil {
		tl = &sbxv1.WakeTimeline{}
		ws.Status.Timeline = tl
	}
	if d.GetState() == sidecarv1.DepsState_DEPS_STATE_READY {
		tl.DepsReadyAt = &metav1.MicroTime{Time: readyAt}
	}
	logger(ctx, ws).Info("dependencies settled", "profile", d.GetProfile(), "source", cur.Source, "reason", reason)
	return true, func() { r.observeDeps(d, tl) }, false
}

func (r *Reconciler) observeDeps(d *sidecarv1.Deps, tl *sbxv1.WakeTimeline) {
	if r.Metrics == nil || d.GetProfile() == "" {
		return
	}
	source := metrics.DepsSourceFailed
	switch d.GetSource() {
	case sidecarv1.DepsSource_DEPS_SOURCE_CACHE:
		source = metrics.DepsSourceCache
		r.Metrics.DepsBytes.Observe(float64(d.GetBytes()))
	case sidecarv1.DepsSource_DEPS_SOURCE_INSTALL:
		source = metrics.DepsSourceInstall
	case sidecarv1.DepsSource_DEPS_SOURCE_PRESENT:
		source = metrics.DepsSourcePresent
	}
	if d.GetState() == sidecarv1.DepsState_DEPS_STATE_FAILED {
		source = metrics.DepsSourceFailed
	}
	r.Metrics.DepsRestores.WithLabelValues(d.GetProfile(), source).Inc()
	if tl.DepsReadyAt != nil && tl.RequestedAt != nil {
		r.Metrics.WakeDuration.WithLabelValues(metrics.StageDepsReady, string(tl.Trigger)).Observe(tl.DepsReadyAt.Sub(tl.RequestedAt.Time).Seconds())
	}
}

func depsCondition(d *sidecarv1.Deps) (metav1.ConditionStatus, string, string) {
	p, _ := depcache.Lookup(d.GetProfile())
	switch d.GetState() {
	case sidecarv1.DepsState_DEPS_STATE_RESTORING:
		return metav1.ConditionFalse, sbxv1.DepsReasonRestoring, fmt.Sprintf("restoring %s (%s) from the cache", p.Dir, p.Name)
	case sidecarv1.DepsState_DEPS_STATE_INSTALLING:
		return metav1.ConditionFalse, sbxv1.DepsReasonInstalling, "running " + p.Install
	case sidecarv1.DepsState_DEPS_STATE_FAILED:
		return metav1.ConditionFalse, sbxv1.DepsReasonFailed, d.GetMessage()
	case sidecarv1.DepsState_DEPS_STATE_READY:
		switch d.GetSource() {
		case sidecarv1.DepsSource_DEPS_SOURCE_NONE:
			return metav1.ConditionTrue, sbxv1.DepsReasonReady, "no dependencies to restore"
		case sidecarv1.DepsSource_DEPS_SOURCE_PRESENT:
			return metav1.ConditionTrue, sbxv1.DepsReasonReady, p.Dir + " was already in the workspace"
		case sidecarv1.DepsSource_DEPS_SOURCE_CACHE:
			return metav1.ConditionTrue, sbxv1.DepsReasonReady, fmt.Sprintf("restored %s from the cache", p.Dir)
		}
		return metav1.ConditionTrue, sbxv1.DepsReasonReady, "installed with " + p.Install
	}
	return metav1.ConditionFalse, sbxv1.DepsReasonPending, "starting the restore"
}

func depsSource(s sidecarv1.DepsSource) string {
	switch s {
	case sidecarv1.DepsSource_DEPS_SOURCE_NONE:
		return sbxv1.DepsSourceNone
	case sidecarv1.DepsSource_DEPS_SOURCE_PRESENT:
		return sbxv1.DepsSourcePresent
	case sidecarv1.DepsSource_DEPS_SOURCE_CACHE:
		return sbxv1.DepsSourceCache
	case sidecarv1.DepsSource_DEPS_SOURCE_INSTALL:
		return sbxv1.DepsSourceInstall
	}
	return ""
}

func equalDeps(a, b *sbxv1.DepsStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// observeDepsUpload records what a snapshot did with the dependency cache.
func (r *Reconciler) observeDepsUpload(u *sidecarv1.DepsUpload) {
	if r.Metrics == nil || u == nil {
		return
	}
	switch {
	case u.GetUploaded():
		r.Metrics.DepsUploads.WithLabelValues(metrics.DepsUploaded).Inc()
		r.Metrics.DepsUploadBytes.Observe(float64(u.GetSizeBytes()))
	case u.GetError() != "":
		r.Metrics.DepsUploads.WithLabelValues(metrics.DepsUploadFailed).Inc()
	default:
		r.Metrics.DepsUploads.WithLabelValues(metrics.DepsUploadSkipped).Inc()
	}
}

// --- services ---

// servicesKey identifies spec.services as applied to one epoch.
func servicesKey(ws *sbxv1.Workspace) string {
	if len(ws.Spec.Services) == 0 {
		return ""
	}
	b, _ := json.Marshal(ws.Spec.Services)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("e%d-%s", ws.Status.Epoch, hex.EncodeToString(sum[:8]))
}

// ensureServices starts spec.services in this epoch's pod, and stops the
// ones no longer declared. Services that wait for dependencies start once
// the restore settles, even if it failed: the service then reports why.
// It reports whether status changed.
func (r *Reconciler) ensureServices(ctx context.Context, ws *sbxv1.Workspace, depsPending bool) bool {
	want := servicesKey(ws)
	if ws.Status.ServicesApplied == want {
		return false
	}
	agent := r.Pods.Agent(ws, r.pod(ws))
	rctx, cancel := r.rpcCtx(ctx)
	defer cancel()
	running, err := agent.ListServices(rctx)
	if err != nil {
		logger(ctx, ws).Info("listing services failed; retrying", "err", err.Error())
		return false
	}
	declared := map[string]bool{}
	all := true
	for _, s := range ws.Spec.Services {
		declared[s.Name] = true
		if depsPending && s.WaitsForDeps() {
			all = false
			continue
		}
		spec := &agentv1.Service{Name: s.Name, Command: s.Command, Cwd: s.Cwd, Env: s.Env, NeedsDeps: s.WaitsForDeps()}
		if err := agent.StartService(rctx, spec); err != nil {
			logger(ctx, ws).Info("starting a service failed; retrying", "service", s.Name, "err", err.Error())
			return false
		}
	}
	for _, st := range running {
		if name := st.GetService().GetName(); !declared[name] {
			if err := agent.StopService(rctx, name); err != nil {
				logger(ctx, ws).Info("stopping a service failed; retrying", "service", name, "err", err.Error())
				return false
			}
		}
	}
	if !all {
		return false
	}
	ws.Status.ServicesApplied = want
	logger(ctx, ws).Info("services started", "count", len(ws.Spec.Services))
	return true
}
