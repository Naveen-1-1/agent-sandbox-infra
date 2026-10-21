// Package workspace is the Workspace controller: a level-triggered state
// machine that provisions, suspends and wakes workspaces on top of
// agent-sandbox Sandboxes.
//
// Every step is idempotent and its completion is recorded in status with an
// optimistic-concurrency update, so a controller that crashes after any side
// effect simply redoes the current step. The reconciler keeps no state of
// its own between calls.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Finalizers the controller owns.
const (
	// FinalizerWorkspace lets the controller purge S3 before a Workspace goes.
	FinalizerWorkspace = "sbx.dev/workspace"
	// FinalizerSnapshotProtection keeps a workspace disk until its contents
	// are safely committed to S3.
	FinalizerSnapshotProtection = "sbx.dev/snapshot-protection"
)

// Config is how workspace pods are built and how patient the controller is.
type Config struct {
	RuntimeImage     string
	BrokerAddr       string
	DefaultImage     string
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	RunAsUser        int64
	RunAsGroup       int64
	StorageSize      resource.Quantity
	StorageClassName string
	Resources        corev1.ResourceRequirements
	Snapshot         podspec.Snapshot

	// FreezeLease bounds how long a workload stays frozen if the controller
	// disappears before committing a suspend.
	FreezeLease time.Duration
	// SuspendTimeout aborts a suspend that hasn't committed by then.
	SuspendTimeout time.Duration
	// Poll is how often in-progress work is checked.
	Poll time.Duration
	// RPCTimeout bounds each call to a pod.
	RPCTimeout time.Duration
	// MaxConcurrent bounds parallel reconciles.
	MaxConcurrent int
}

func (c *Config) defaults() {
	if c.FreezeLease <= 0 {
		c.FreezeLease = 2 * time.Minute
	}
	if c.SuspendTimeout <= 0 {
		c.SuspendTimeout = 10 * time.Minute
	}
	if c.Poll <= 0 {
		c.Poll = 500 * time.Millisecond
	}
	if c.RPCTimeout <= 0 {
		c.RPCTimeout = 10 * time.Second
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 16
	}
	if c.StorageSize.IsZero() {
		c.StorageSize = resource.MustParse("5Gi")
	}
}

// Reconciler reconciles Workspaces.
type Reconciler struct {
	Client    client.Client // reads may be cached; writes go to the API
	API       client.Reader // uncached reads, for decisions that must be current
	Scheme    *runtime.Scheme
	Keys      token.Keyring
	Store     *snapstore.Store
	Objects   objstore.Store
	Pods      PodClients
	Inspector Inspector
	Metrics   *metrics.Workspace
	Snapshots *metrics.Snapshot
	Config    Config
	Now       func() time.Time

	// Policies picks each workspace's sleep policy. Without it, workspaces
	// suspend only when asked to.
	Policies *sleep.Policies
	// Gateways is the gateway replicas' traffic view; nil ignores it.
	Gateways GatewaySignals
	// Ops bounds concurrent wakes and suspends; nil means no bound.
	Ops   *ops.Limiter
	Sleep *metrics.Sleep
	// Wakeups, if set, carries workspaces to decide about right away (their
	// gateway signals changed).
	Wakeups <-chan event.GenericEvent
}

// SetupWithManager registers the controller. Sandboxes it owns trigger
// reconciles when their readiness or suspension changes, and a change to
// the policy ConfigMaps re-decides every running workspace. filters, if
// given, limit which objects trigger reconciles.
func (r *Reconciler) SetupWithManager(mgr manager.Manager, filters ...predicate.Predicate) error {
	r.Config.defaults()
	if r.Now == nil {
		r.Now = time.Now
	}
	b := builder.ControllerManagedBy(mgr).
		Named("workspace").
		For(&sbxv1.Workspace{}).
		Owns(&sbv1.Sandbox{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Config.MaxConcurrent})
	for _, f := range filters {
		b = b.WithEventFilter(f)
	}
	if r.Policies != nil {
		// A policy change re-decides every running workspace at once.
		b = b.Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.runningWorkspaces),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isPolicyConfig)))
	}
	if r.Wakeups != nil {
		b = b.WatchesRawSource(source.Channel(r.Wakeups, &handler.EnqueueRequestForObject{}))
	}
	return b.Complete(r)
}

func (r *Reconciler) isPolicyConfig(o client.Object) bool {
	return o.GetNamespace() == r.Policies.Namespace &&
		(o.GetName() == sleep.PolicyConfigMap || o.GetName() == sleep.DefaultsConfigMap)
}

func (r *Reconciler) runningWorkspaces(ctx context.Context, _ client.Object) []reconcile.Request {
	// Load the new config now, so a bad one is reported even if no
	// workspace is running to need it.
	_, _ = r.Policies.Cluster(ctx)
	list := &sbxv1.WorkspaceList{}
	if err := r.Client.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing workspaces after a policy change")
		return nil
	}
	var out []reconcile.Request
	for _, ws := range list.Items {
		if ws.Status.Phase == sbxv1.PhaseRunning {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ws)})
		}
	}
	return out
}

// Reconcile moves one workspace one step through its lifecycle.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	r.Config.defaults()
	if r.Now == nil {
		r.Now = time.Now
	}
	ws := &sbxv1.Workspace{}
	if err := r.Client.Get(ctx, req.NamespacedName, ws); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ws.Annotations[sbxv1.AnnotationPaused] == "true" {
		return reconcile.Result{}, nil
	}
	if !ws.DeletionTimestamp.IsZero() {
		r.forget(ws)
		return r.finalize(ctx, ws)
	}
	if !controllerutil.ContainsFinalizer(ws, FinalizerWorkspace) {
		controllerutil.AddFinalizer(ws, FinalizerWorkspace)
		return reconcile.Result{}, ignoreConflict(r.Client.Update(ctx, ws))
	}

	// Operations in progress keep their slot; this also re-counts them
	// after a controller restart.
	switch ws.Status.Phase {
	case sbxv1.PhaseProvisioning, sbxv1.PhaseWaking:
		if ws.Status.Wake != nil {
			r.hold(ws, r.wakeClass(ws, ws.Status.Wake.Trigger))
		}
	case sbxv1.PhaseSuspending, sbxv1.PhaseTearingDown:
		r.hold(ws, ops.Suspend)
	default:
		r.release(ws) // settled; a queued request for a new operation stays queued
	}

	switch ws.Status.Phase {
	case "", sbxv1.PhasePending:
		return r.startProvisioning(ctx, ws)
	case sbxv1.PhaseProvisioning, sbxv1.PhaseWaking:
		return r.bringUp(ctx, ws)
	case sbxv1.PhaseRunning:
		return r.running(ctx, ws)
	case sbxv1.PhaseSuspending:
		return r.suspending(ctx, ws)
	case sbxv1.PhaseTearingDown:
		return r.tearDown(ctx, ws)
	case sbxv1.PhaseSuspended:
		return r.suspended(ctx, ws)
	}
	return reconcile.Result{}, nil
}

// save writes status with optimistic concurrency; after runs only if the
// write succeeded (so metrics aren't double counted on a conflict).
func (r *Reconciler) save(ctx context.Context, ws *sbxv1.Workspace, after ...func()) (reconcile.Result, error) {
	ws.Status.ObservedGeneration = ws.Generation
	if err := r.Client.Status().Update(ctx, ws); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{RequeueAfter: 100 * time.Millisecond}, nil
		}
		return reconcile.Result{}, err
	}
	for _, f := range after {
		f()
	}
	return reconcile.Result{}, nil
}

// setPhase moves ws to phase and returns the hook that records the time
// spent in the old phase.
func (r *Reconciler) setPhase(ws *sbxv1.Workspace, phase sbxv1.Phase) func() {
	old, since, now := ws.Status.Phase, ws.Status.PhaseSince, r.Now()
	if old == phase {
		return func() {}
	}
	ws.Status.Phase = phase
	ws.Status.PhaseSince = &metav1.Time{Time: now}
	return func() {
		if r.Metrics != nil && since != nil {
			r.Metrics.ObservePhase(string(old), since.Time, now)
		}
	}
}

func setCondition(ws *sbxv1.Workspace, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: ws.Generation,
	})
}

func (r *Reconciler) requeue() (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: r.Config.Poll}, nil
}

func (r *Reconciler) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.Config.RPCTimeout)
}

func ignoreConflict(err error) error {
	if apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// Names of the objects one workspace epoch owns.
func claimName(ws *sbxv1.Workspace, epoch int64) string {
	return fmt.Sprintf("ws-%s-e%d", ws.Name, epoch)
}
func secretName(ws *sbxv1.Workspace, epoch int64) string {
	return fmt.Sprintf("%s-e%d-tokens", ws.Name, epoch)
}
func sandboxName(ws *sbxv1.Workspace) string { return ws.Name }

func newAttemptID(epoch int64) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("e%d-%s", epoch, hex.EncodeToString(b))
}

func logger(ctx context.Context, ws *sbxv1.Workspace) logr.Logger {
	return log.FromContext(ctx).WithValues("workspace", client.ObjectKeyFromObject(ws), "phase", ws.Status.Phase)
}
