/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// OADPInstallReconciler ensures the OADP OperatorGroup & Subscription
// exist and are configured properly, and can restart the pod itself
// if we need to reload after veleroCRDs get installed (either by the OADP
// subscription or manually)
type OADPInstallReconciler struct {
	client.Client
	Log                          logr.Logger
	Scheme                       *runtime.Scheme
	RequiredCrdsPresentAtStartup bool
}

const crdCheckInterval = time.Minute * 10

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *OADPInstallReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	crdsPresent, err := EnsureOADPInstallAndCRDs(ctx, r.Log, r.Client,
		r.RequiredCrdsPresentAtStartup /* skip CRD check if the CRDS already present at startup */)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !r.RequiredCrdsPresentAtStartup {
		if crdsPresent {
			// exit so pod will restart and start our other controllers that
			// depend on the CRDs
			r.Log.Info("Required CRDs are now found - exiting to restart the cluster-backup-operator")
			os.Exit(0)
		}

		// CRDs still aren't present - requeue to periodically check on them
		r.Log.Info("Velero CRDs are not installed, will requeue after interval", "crdCheckInterval", crdCheckInterval)
		return ctrl.Result{RequeueAfter: crdCheckInterval}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OADPInstallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	/*
		if err := mgr.GetFieldIndexer().IndexField(
			context.Background(),
			&veleroapi.Schedule{},
			scheduleOwnerKey,
			func(rawObj client.Object) []string {
				schedule := rawObj.(*veleroapi.Schedule)
				owner := metav1.GetControllerOf(schedule)
				if owner == nil || owner.APIVersion != apiGVString || owner.Kind != "BackupSchedule" {
					return nil
				}

				return []string{owner.Name}
			}); err != nil {
			return err
		}
	*/

	// Print out some debug info at startup
	setupLog := ctrl.Log.WithName("oadpinstall_controller_setup")
	setupLog.Info("ClusterSTS", "isClusterSTSEnabled", isClusterSTSEnabled())
	if !shouldEnsureOADPInstall() {
		setupLog.Info("ACM install of OADP reconciling is disabled")
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("oadpinstall_controller").
		For(&corev1.ConfigMap{}, builder.WithPredicates(acmOADPSubsConfigMapFilterPredicate())).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
		}).
		/*
			Owns(&veleroapi.Schedule{}).
			WithEventFilter(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					// Ignore updates to CR status in which case metadata.Generation does not change
					return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
				},
			}).
		*/
		Complete(r)
}

func acmOADPSubsConfigMapFilterPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			cm, ok := e.Object.(*corev1.ConfigMap)
			if !ok {
				return false
			}
			return isACMOADPSubConfigMap(cm)
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			// Do not reconcile on delete
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			cm, ok := e.ObjectNew.(*corev1.ConfigMap)
			if !ok {
				return false
			}
			return isACMOADPSubConfigMap(cm)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			cm, ok := e.Object.(*corev1.ConfigMap)
			if !ok {
				return false
			}
			return isACMOADPSubConfigMap(cm)
		},
	}
}

// Returns true if this is the ConfigMap created at install time (see charts in multiclusterhub)
// that contains params to be used/ in our operator subscription for OADP
func isACMOADPSubConfigMap(cm *corev1.ConfigMap) bool {
	return cm.GetNamespace() == getClusterBackupNamespace() &&
		cm.GetName() == acmOADPOperatorSubsConfigMapName
}
