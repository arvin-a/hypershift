package hcpstatus

import (
	"context"
	"fmt"
	"reflect"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	hyperv1 "github.com/openshift/hypershift/api/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/common"
	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/operator"
	"github.com/openshift/hypershift/support/releaseinfo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const ControllerName = "hcpstatus"

func Setup(opts *operator.HostedClusterConfigOperatorConfig) error {
	r := &hcpStatusReconciler{
		mgtClusterClient:    opts.CPCluster.GetClient(),
		hostedClusterClient: opts.Manager.GetClient(),
		releaseProvider:     opts.ReleaseProvider,
	}
	c, err := controller.New(ControllerName, opts.Manager, controller.Options{Reconciler: r})
	if err != nil {
		return fmt.Errorf("failed to construct controller: %w", err)
	}
	if err := c.Watch(source.NewKindWithCache(&hyperv1.HostedControlPlane{}, opts.CPCluster.GetCache()), &handler.EnqueueRequestForObject{}); err != nil {
		return fmt.Errorf("failed to watch HCP: %w", err)
	}

	clusterVersionMapper := func(crclient.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: opts.Namespace, Name: opts.HCPName}}}
	}
	if err := c.Watch(&source.Kind{Type: &configv1.ClusterVersion{}}, handler.EnqueueRequestsFromMapFunc(clusterVersionMapper)); err != nil {
		return fmt.Errorf("failed to watch clusterversion: %w", err)
	}

	return nil
}

type hcpStatusReconciler struct {
	mgtClusterClient    crclient.Client
	hostedClusterClient crclient.Client
	releaseProvider     releaseinfo.Provider
}

func (h *hcpStatusReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	hcp := &hyperv1.HostedControlPlane{}
	if err := h.mgtClusterClient.Get(ctx, req.NamespacedName, hcp); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get hcp %s: %w", req, err)
	}
	originalHCP := hcp.DeepCopy()
	if err := h.reconcile(ctx, hcp); err != nil {
		return reconcile.Result{}, err
	}

	if !reflect.DeepEqual(hcp.Status, originalHCP.Status) {
		if err := h.mgtClusterClient.Status().Update(ctx, hcp); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to update hcp: %w", err)
		}
	}

	return reconcile.Result{}, nil
}

// findClusterOperatorStatusCondition is identical to meta.FindStatusCondition except that it works on config1.ClusterOperatorStatusCondition instead of
// metav1.StatusCondition
func findClusterOperatorStatusCondition(conditions []configv1.ClusterOperatorStatusCondition, conditionType configv1.ClusterStatusConditionType) *configv1.ClusterOperatorStatusCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}

	return nil
}

func (h *hcpStatusReconciler) reconcile(ctx context.Context, hcp *hyperv1.HostedControlPlane) error {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling hosted cluster version conditions")

	var clusterVersion configv1.ClusterVersion
	err := h.hostedClusterClient.Get(ctx, crclient.ObjectKey{Name: "version"}, &clusterVersion)
	// We check err in loop below to build conditions with ConditionUnknown status for all types.

	cvoConditions := map[hyperv1.ConditionType]*configv1.ClusterOperatorStatusCondition{
		hyperv1.ClusterVersionFailing:         findClusterOperatorStatusCondition(clusterVersion.Status.Conditions, "Failing"),
		hyperv1.ClusterVersionReleaseAccepted: findClusterOperatorStatusCondition(clusterVersion.Status.Conditions, "ReleaseAccepted"),
		hyperv1.ClusterVersionProgressing:     findClusterOperatorStatusCondition(clusterVersion.Status.Conditions, configv1.OperatorProgressing),
		hyperv1.ClusterVersionUpgradeable:     findClusterOperatorStatusCondition(clusterVersion.Status.Conditions, configv1.OperatorUpgradeable),
		hyperv1.ClusterVersionAvailable:       findClusterOperatorStatusCondition(clusterVersion.Status.Conditions, configv1.OperatorAvailable),
	}

	for conditionType := range cvoConditions {
		var hcpCVOCondition metav1.Condition
		// Set unknown status.
		var unknownStatusMessage string
		if cvoConditions[conditionType] == nil {
			unknownStatusMessage = "Condition not found in the CVO."
		}
		if err != nil {
			unknownStatusMessage = fmt.Sprintf("failed to get clusterVersion: %v", err)
		}

		hcpCVOCondition = metav1.Condition{
			Type:               string(conditionType),
			Status:             metav1.ConditionUnknown,
			Reason:             hyperv1.StatusUnknownReason,
			Message:            unknownStatusMessage,
			ObservedGeneration: hcp.Generation,
		}

		if err == nil && cvoConditions[conditionType] != nil {
			// Bubble up info from CVO.
			reason := cvoConditions[conditionType].Reason
			// reason is not required in ClusterOperatorStatusCondition, but it's in metav1.conditions.
			// So we need to make sure the input does not break the KAS expectation.
			if reason == "" {
				reason = hyperv1.FromClusterVersionReason
			}
			hcpCVOCondition = metav1.Condition{
				Type:               string(conditionType),
				Status:             metav1.ConditionStatus(cvoConditions[conditionType].Status),
				Reason:             reason,
				Message:            cvoConditions[conditionType].Message,
				ObservedGeneration: hcp.Generation,
			}
		}

		meta.SetStatusCondition(&hcp.Status.Conditions, hcpCVOCondition)
	}
	log.Info("Finished reconciling hosted cluster version conditions")

	// If a rollout is in progress, compute and record the rollout status. The
	// image version will be considered rolled out if the hosted CVO reports
	// having completed the rollout of the semantic version matching the release
	// image specified on the HCP.
	if hcp.Status.ReleaseImage != hcp.Spec.ReleaseImage {
		releaseImage, err := h.lookupReleaseImage(ctx, hcp)
		if err != nil {
			return fmt.Errorf("failed to look up release image: %w", err)
		}

		timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var clusterVersion configv1.ClusterVersion
		if err := h.hostedClusterClient.Get(timeout, crclient.ObjectKey{Name: "version"}, &clusterVersion); err != nil {
			log.Info("failed to get clusterversion, can't determine image version rollout status", "error", err)
		} else {
			versionHistory := clusterVersion.Status.History
			if len(versionHistory) > 0 &&
				versionHistory[0].Version == releaseImage.Version() &&
				versionHistory[0].State == configv1.CompletedUpdate {
				// Rollout to the desired release image version is complete, so record
				// that fact on the HCP status.
				now := metav1.NewTime(time.Now())
				hcp.Status.ReleaseImage = hcp.Spec.ReleaseImage
				hcp.Status.Version = releaseImage.Version()
				hcp.Status.LastReleaseImageTransitionTime = &now
			}
		}
	}

	return nil
}

func (h *hcpStatusReconciler) lookupReleaseImage(ctx context.Context, hcp *hyperv1.HostedControlPlane) (*releaseinfo.ReleaseImage, error) {
	pullSecret := common.PullSecret(hcp.Namespace)
	if err := h.mgtClusterClient.Get(ctx, crclient.ObjectKeyFromObject(pullSecret), pullSecret); err != nil {
		return nil, err
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer lookupCancel()
	return h.releaseProvider.Lookup(lookupCtx, hcp.Spec.ReleaseImage, pullSecret.Data[corev1.DockerConfigJsonKey])
}
