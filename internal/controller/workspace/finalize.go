package workspace

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
)

// finalize removes everything a deleted workspace owns: its Sandbox, disks
// and token Secrets (explicitly, rather than waiting for garbage
// collection), and its S3 prefix. Cached base bundles are shared and stay.
func (r *Reconciler) finalize(ctx context.Context, ws *sbxv1.Workspace) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(ws, FinalizerWorkspace) {
		return reconcile.Result{}, nil
	}
	if ws.Status.Phase != sbxv1.PhaseDeleting {
		_, err := r.save(ctx, ws, r.setPhase(ws, sbxv1.PhaseDeleting))
		return reconcile.Result{RequeueAfter: r.Config.Poll}, err
	}

	sb := &sbv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: sandboxName(ws)}}
	if err := r.Client.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	selector := client.MatchingLabels{podspec.LabelWorkspace: ws.Name}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.API.List(ctx, claims, client.InNamespace(ws.Namespace), selector); err != nil {
		return reconcile.Result{}, err
	}
	for i := range claims.Items {
		if !metav1.IsControlledBy(&claims.Items[i], ws) {
			continue
		}
		if _, err := r.deleteClaim(ctx, ws, claims.Items[i].Name); err != nil {
			return reconcile.Result{}, err
		}
	}
	secrets := &corev1.SecretList{}
	if err := r.API.List(ctx, secrets, client.InNamespace(ws.Namespace), selector); err != nil {
		return reconcile.Result{}, err
	}
	for i := range secrets.Items {
		if metav1.IsControlledBy(&secrets.Items[i], ws) {
			if err := r.Client.Delete(ctx, &secrets.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return reconcile.Result{}, err
			}
		}
	}

	if r.Objects != nil {
		objs, err := r.Objects.List(ctx, "ws/"+string(ws.UID)+"/")
		if err != nil {
			return reconcile.Result{}, err
		}
		for _, o := range objs {
			if err := r.Objects.Delete(ctx, o.Key); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(ws, FinalizerWorkspace)
	return reconcile.Result{}, ignoreConflict(client.IgnoreNotFound(r.Client.Update(ctx, ws)))
}
