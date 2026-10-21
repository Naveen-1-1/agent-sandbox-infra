// Package fakekubelet stands in for the kubelet and scheduler in envtest,
// where pods otherwise stay Pending forever and are never really deleted.
// It marks new pods Running and Ready with a fake IP, and finishes pod
// deletions immediately.
package fakekubelet

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Kubelet is the fake. Its counters let tests assert how many pods ran.
type Kubelet struct {
	Client client.Client
	// Gate, if set, holds a pod Pending until it returns true, standing in
	// for init containers that wait on something (e.g. the broker).
	Gate    func(ctx context.Context, pod *corev1.Pod) bool
	started atomic.Int64
	nextIP  atomic.Int64
}

// Setup registers the fake with mgr.
func Setup(mgr manager.Manager) (*Kubelet, error) {
	k := &Kubelet{Client: mgr.GetClient()}
	return k, ctrl.ControllerManagedBy(mgr).Named("fake-kubelet").For(&corev1.Pod{}).Complete(k)
}

// Started counts pods the fake has started.
func (k *Kubelet) Started() int64 { return k.started.Load() }

// Reconcile starts or finishes deleting one pod.
func (k *Kubelet) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	pod := &corev1.Pod{}
	if err := k.Client.Get(ctx, req.NamespacedName, pod); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !pod.DeletionTimestamp.IsZero() {
		if len(pod.Finalizers) > 0 {
			return reconcile.Result{}, nil // someone is holding it on purpose
		}
		err := k.Client.Delete(ctx, pod, client.GracePeriodSeconds(0))
		if apierrors.IsNotFound(err) {
			err = nil
		}
		return reconcile.Result{}, err
	}
	if pod.Status.Phase == corev1.PodRunning {
		return reconcile.Result{}, nil
	}
	if k.Gate != nil && !k.Gate(ctx, pod) {
		return reconcile.Result{RequeueAfter: 20 * time.Millisecond}, nil
	}

	now := metav1.NewTime(time.Now())
	ip := fmt.Sprintf("10.244.%d.%d", (k.nextIP.Add(1)/250)%250, k.nextIP.Load()%250+1)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = ip
	pod.Status.PodIPs = []corev1.PodIP{{IP: ip}}
	pod.Status.StartTime = &now
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	pod.Status.InitContainerStatuses = nil
	for _, c := range pod.Spec.InitContainers {
		st := corev1.ContainerStatus{Name: c.Name, Image: c.Image}
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			st.Ready, st.Started = true, ptr(true)
			st.State.Running = &corev1.ContainerStateRunning{StartedAt: now}
		} else {
			st.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", StartedAt: now, FinishedAt: now}
		}
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, st)
	}
	pod.Status.ContainerStatuses = nil
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, ImageID: c.Image + "@sha256:fake", Ready: true, Started: ptr(true),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}},
		})
	}
	if err := k.Client.Status().Update(ctx, pod); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return reconcile.Result{RequeueAfter: 20 * time.Millisecond}, nil
		}
		return reconcile.Result{}, err
	}
	k.started.Add(1)
	return reconcile.Result{}, nil
}

func ptr[T any](v T) *T { return &v }
