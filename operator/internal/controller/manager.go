package controller

import (
	csiv1alpha1 "github.com/truenas/truenas-csi/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func ignoreDeletionPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates to CR status in which case metadata.Generation does not change
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Evaluates to false if the object has been confirmed deleted.
			return !e.DeleteStateUnknown
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TrueNASCSIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Only react to spec changes on the CR itself. Status writes and the
		// finalizer update do not bump metadata.generation, so this predicate
		// stops the controller from re-triggering itself. Steady-state drift is
		// still caught by the RequeueAfterRunning poll and by the Owns watches.
		For(&csiv1alpha1.TrueNASCSI{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// The Owns watches deliberately carry no predicate: updateStatusRunning
		// reads ReadyReplicas and NumberReady, so it must see status-only
		// changes on the workloads.
		Owns(&appsv1.Deployment{}, builder.WithPredicates(ignoreDeletionPredicate())).
		Owns(&appsv1.DaemonSet{}, builder.WithPredicates(ignoreDeletionPredicate())).
		Owns(&corev1.ConfigMap{}, builder.WithPredicates(ignoreDeletionPredicate())).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&storagev1.CSIDriver{}).
		Owns(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "security.openshift.io/v1", "kind": "SecurityContextConstraints"}}).
		Named("truenascsi").
		Complete(r)
}
