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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
)

const (
	ENVVAR_CLUSTER_STS_ENABLED string = "CLUSTER_STS_ENABLED"
	ENVVAR_POD_NAMESPACE       string = "POD_NAMESPACE"

	operatorGroupName               string = "redhat-oadp-operator-group"
	acmOADPOperatorSubscriptionName string = "redhat-oadp-operator-subscription"

	// Config map created at install time (see charts in multiclusterhub) that contains params to be used
	// in our operator subscription for OADP
	acmOADPOperatorSubsConfigMapName string = "acm-redhat-oadp-operator-subscription"

	//TODO: letting this get set in the installer chart from values instead - DefaultMinOADPChannelVersion string = "1.4"
)

// Use GetClusterBackupNamespace() to access this
var clusterBackupNamespace = ""

//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=operators.coreos.com,resources=operatorgroups,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions,verbs=get;list;watch;create;update;patch;delete

// Reconciles the OADP operatorgroup & subscription (unless disabled)
// returns true if CRDs are available
func EnsureOADPInstallAndCRDs(
	ctx context.Context,
	logger logr.Logger,
	k8sClient client.Client,
	skipCRDCheck bool,
) (bool, error) {
	if shouldEnsureOADPInstall() {
		err := ensureOADPInstall(ctx, logger, k8sClient)
		if err != nil {
			return false, err
		}
	}

	if skipCRDCheck {
		return true, nil
	}

	return VeleroCRDsPresent(ctx, k8sClient)
}

func shouldEnsureOADPInstall() bool {
	//TODO: could also allow another way of disabling cluster-backup from deploying OADP
	return !isClusterSTSEnabled() // Do not deploy OADP if cluster STS is enabled
}

func isClusterSTSEnabled() bool {
	stsEnabledStr := os.Getenv(ENVVAR_CLUSTER_STS_ENABLED)
	if stsEnabledStr == "1" || stsEnabledStr == "true" {
		return true
	}
	return false
}

func getClusterBackupNamespace() string {
	if clusterBackupNamespace == "" {
		// Just do this lookup once
		clusterBackupNamespace = os.Getenv(ENVVAR_POD_NAMESPACE)
	}
	return clusterBackupNamespace
}

func ensureOADPInstall(
	ctx context.Context,
	logger logr.Logger,
	k8sClient client.Client,
) error {
	// Ensure OADP OperatorGroup
	err := ensureOperatorGroup(ctx, logger, k8sClient)
	if err != nil {
		return err
	}

	// Ensure OADP Subscription
	return ensureOADPOperatorSubscription(ctx, logger, k8sClient)
}

func ensureOperatorGroup(
	ctx context.Context,
	logger logr.Logger,
	k8sClient client.Client,
) error {
	// Only 1 operatorgroup is allowed per namespace - if one exists at all
	// we'll assume it's ok (could be manually created by the user)
	operatorGroupList := &operatorsv1.OperatorGroupList{}
	err := k8sClient.List(ctx, operatorGroupList, client.InNamespace(getClusterBackupNamespace()))
	if err != nil {
		logger.Error(err, "Unable to query for OperatorGroups", "namespace", getClusterBackupNamespace())
		return err
	}

	if len(operatorGroupList.Items) >= 0 {
		// Nothing needed here - doublecheck that the spec is set correctly
		existingOperatorGroup := operatorGroupList.Items[0]
		if len(existingOperatorGroup.Spec.TargetNamespaces) != 1 ||
			existingOperatorGroup.Spec.TargetNamespaces[0] != getClusterBackupNamespace() {
			// Just log a warning message
			logger.Info("Warning, OperatorGroup does not have targetNamespaces set to expected value",
				"OperatorGroup name", existingOperatorGroup.GetName(),
				"OperatorGroup namespace", getClusterBackupNamespace(),
				"targetNamespaces", existingOperatorGroup.Spec.TargetNamespaces,
				"Expected targetNamespaces", []string{getClusterBackupNamespace()})
		}

		return nil
	}

	// No OperatorGroup exists, create one
	operatorGroup := &operatorsv1.OperatorGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      operatorGroupName,
			Namespace: getClusterBackupNamespace(),
		},
		Spec: operatorsv1.OperatorGroupSpec{
			TargetNamespaces: []string{
				getClusterBackupNamespace(),
			},
		},
	}

	return k8sClient.Create(ctx, operatorGroup)
}

func ensureOADPOperatorSubscription(
	ctx context.Context,
	logger logr.Logger,
	k8sClient client.Client,
) error {
	// Get ConfigMap that contains the subscription configuration ACM should use
	subsConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      acmOADPOperatorSubsConfigMapName,
			Namespace: getClusterBackupNamespace(),
		},
	}
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(subsConfigMap), subsConfigMap)
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Info("configmap with OADP operator subscription details is missing. "+
				"OADP operator subscription will not be reconciled.",
				"ConfigMap name", acmOADPOperatorSubsConfigMapName)
			return nil
		}
		logger.Error(err, "unable to get configmap with OADP operator subscription details",
			"ConfigMap name", acmOADPOperatorSubsConfigMapName)
		return err
	}

	subsConfig := subscriptionConfig{subsConfigMap}

	// See if a subscription for OADP already exists
	subscriptionList := &operatorsv1alpha1.SubscriptionList{}
	err = k8sClient.List(ctx, subscriptionList, client.InNamespace(getClusterBackupNamespace()))
	if err != nil {
		logger.Error(err, "unable to query for operator subscriptions", "namespace", getClusterBackupNamespace())
		return err
	}

	var acmCreatedOperatorSubscription *operatorsv1alpha1.Subscription
	var userCreatedOperatorSubscription *operatorsv1alpha1.Subscription
	for i := range subscriptionList.Items {
		subs := subscriptionList.Items[i]
		if subs.GetName() == acmOADPOperatorSubscriptionName {
			acmCreatedOperatorSubscription = &subs
		} else if subs.Spec.Package == subsConfig.getPackage() { // OADP operator name
			userCreatedOperatorSubscription = &subs
		}
	}

	if userCreatedOperatorSubscription != nil {
		logger.Info("User created OADP operator subscription exists, ACM will not reconcile or alter the subscription.",
			"operator subscription name", userCreatedOperatorSubscription.GetName())
		// There is already an operator subscription in the namespace for OADP
		// We will assume this is what the user wants and remove our ACM
		// created OADP operator subscription
		if acmCreatedOperatorSubscription != nil {
			logger.Info("Removing ACM created operator subscription")
			return k8sClient.Delete(ctx, acmCreatedOperatorSubscription)
		}

		// Do not reconcile the user created operator subscription
		return nil
	}

	// Reconcile our acm-created OADP operator subscription
	return createOrUpdateOADPSubscription(ctx, logger, k8sClient, subsConfig)
}

func createOrUpdateOADPSubscription(
	ctx context.Context,
	logger logr.Logger,
	k8sClient client.Client,
	subsConfig subscriptionConfig,
) error {
	oadpSubscription := &operatorsv1alpha1.Subscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:      acmOADPOperatorSubscriptionName,
			Namespace: getClusterBackupNamespace(),
		},
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, k8sClient, oadpSubscription, func() error {
		/*
			if err := ctrl.SetControllerReference(d.Owner, d.role, d.Client.Scheme()); err != nil {
				logger.Error(err, ErrUnableToSetControllerRef)
				return err
			}
		*/

		if oadpSubscription.Spec == nil {
			oadpSubscription.Spec = &operatorsv1alpha1.SubscriptionSpec{}
		}

		oadpSubscription.Spec.Package = subsConfig.getPackage()
		oadpSubscription.Spec.Channel = subsConfig.getChannel()
		oadpSubscription.Spec.CatalogSource = subsConfig.getCatalogSource()
		oadpSubscription.Spec.CatalogSourceNamespace = subsConfig.getCatalogSourceNamespace()
		oadpSubscription.Spec.InstallPlanApproval = subsConfig.getInstallPlanApproval()

		//TODO: oadpSubscription.Spec.Config

		return nil
	})
	if err != nil {
		logger.Error(err, "operator subscription for OADP reconcile failed")
		return err
	}

	logger.Info("operator subscription for OADP reconcile successful", "operation", op)
	return nil
}

type subscriptionConfig struct {
	// Configmap created by installer charts containing the desired
	// OADP operator subscription details
	cm *corev1.ConfigMap
}

// This is the operator name (aka "Package")
func (sc subscriptionConfig) getName() string {
	return sc.cm.Data["name"]
}
func (sc subscriptionConfig) getPackage() string {
	return sc.getName() // "Package" in subscriptionspec struct == "name" in API
}
func (sc subscriptionConfig) getChannel() string {
	return sc.cm.Data["channel"]
}
func (sc subscriptionConfig) getCatalogSource() string {
	return sc.cm.Data["source"]
}
func (sc subscriptionConfig) getCatalogSourceNamespace() string {
	return sc.cm.Data["sourceNamespace"]
}
func (sc subscriptionConfig) getInstallPlanApproval() operatorsv1alpha1.Approval {
	approvalStr := sc.cm.Data["installPlanApproval"]
	if approvalStr == "" {
		// default if not set
		return operatorsv1alpha1.ApprovalAutomatic
	}
	return operatorsv1alpha1.Approval(approvalStr)
}

//FIXME: subscriptionConfig.getConfig() (http proxy settings, tolerations etc)
