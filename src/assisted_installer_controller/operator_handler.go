package assisted_installer_controller

import (
	"context"

	olmv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
	batchV1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/assisted-installer/src/k8s_client"
	"github.com/openshift/assisted-installer/src/utils"
	"github.com/openshift/assisted-service/models"
)

const (
	cvoOperatorName = "cvo"
	olmNamespace    = "openshift-marketplace"
)

type OperatorHandler interface {
	GetName() string
	GetStatus() (models.OperatorStatus, string, string, error)
	OnChange(newStatus models.OperatorStatus) bool
	IsInitialized() bool
}

// checkAndUpdateOperatorAvailability checks performs the following actions:
//  1. Checks the current availability status of the operator object in assisted-service.
//     If it is available, it returns true, otherwise continue to check the operator status in the cluster.
//  2. If the status or message differs from the current values, it updates the operator's object status in assisted-service.
//  3. Returns true if the status changed to 'available', or false otherwise.
func (c *controller) checkAndUpdateOperatorAvailability(handler OperatorHandler, useCache bool) bool {
	operatorName := handler.GetName()
	operatorStatusInService, isAvailable := c.isOperatorAvailableInService(operatorName, c.OpenshiftVersion, useCache)
	c.log.Infof("Checking availability of operator <%s>: %v", operatorName, isAvailable)
	if isAvailable {
		return true
	}

	operatorStatus, operatorMessage, operatorVersion, err := handler.GetStatus()
	if err != nil {
		c.log.WithError(err).Warnf("Failed to get <%s> operator", operatorName)
		return false
	}

	if operatorStatusInService != nil &&
		(operatorStatusInService.Status != operatorStatus ||
			(operatorStatusInService.StatusInfo != operatorMessage && operatorMessage != "")) {
		c.log.Infof(
			"Operator <%s> updated, status: %s -> %s, message: %s -> %s.",
			operatorName,
			operatorStatusInService.Status,
			operatorStatus,
			operatorStatusInService.StatusInfo,
			operatorMessage,
		)
		if !handler.OnChange(operatorStatus) {
			c.log.WithError(err).Warnf("<%s> operator's OnChange() returned false. Will skip an update.", operatorName)
			return false
		}

		err = c.ic.UpdateClusterOperator(context.TODO(), c.ClusterID, operatorName, operatorVersion, operatorStatus, operatorMessage)
		if err != nil {
			c.log.WithError(err).Warnf("Failed to update %s operator status %s with message %s", operatorName, operatorStatus, operatorMessage)
			return false
		}
		// In case operator was available we should return ealier in order finish quicker.
		c.log.Infof("Operator %s, status %s", operatorName, operatorStatus)
		isAvailable = operatorStatus == models.OperatorStatusAvailable
	}

	return isAvailable
}

func (c *controller) isOperatorAvailableInService(operatorName string, openshiftVersion string, useCache bool) (*models.MonitoredOperator, bool) {
	operatorStatusInService, err := c.ic.GetClusterMonitoredOperator(utils.GenerateRequestContext(), c.ClusterID, operatorName, openshiftVersion, useCache)
	if err != nil {
		c.log.WithError(err).Errorf("Failed to get cluster %s %s operator status", c.ClusterID, operatorName)
		return nil, false
	}

	if operatorStatusInService.Status == models.OperatorStatusAvailable {
		c.log.Infof("Service acknowledged <%s> operator is available for cluster %s", operatorName, c.ClusterID)
		return operatorStatusInService, true
	}

	return operatorStatusInService, false
}

type ClusterOperatorHandler struct {
	kc           k8s_client.K8SClient
	operatorName string
	log          *logrus.Logger
}

func NewClusterOperatorHandler(kc k8s_client.K8SClient, operatorName string, log *logrus.Logger) *ClusterOperatorHandler {
	return &ClusterOperatorHandler{kc: kc, operatorName: operatorName, log: log}
}

func (handler ClusterOperatorHandler) GetName() string { return handler.operatorName }

func (handler ClusterOperatorHandler) IsInitialized() bool { return true }

func (handler ClusterOperatorHandler) GetStatus() (models.OperatorStatus, string, string, error) {
	co, err := handler.kc.GetClusterOperator(handler.operatorName)
	if err != nil {
		return "", "", "", err
	}
	version := ""
	if co != nil && len(co.Status.Versions) > 0 {
		version = co.Status.Versions[0].Version
	}

	handler.log.Infof("%s status conditions: %+v", handler.GetName(), co.Status.Conditions)
	operatorStatus, operatorMessage := utils.MonitoredOperatorStatus(co.Status.Conditions)
	return operatorStatus, operatorMessage, version, nil
}

func (handler ClusterOperatorHandler) OnChange(_ models.OperatorStatus) bool { return true }

type ClusterVersionHandler struct {
	kc  k8s_client.K8SClient
	log *logrus.Logger
}

func NewClusterVersionHandler(log *logrus.Logger, kc k8s_client.K8SClient) *ClusterVersionHandler {
	return &ClusterVersionHandler{kc: kc, log: log}
}

func (handler ClusterVersionHandler) GetName() string { return cvoOperatorName }

func (handler ClusterVersionHandler) IsInitialized() bool { return true }

func (handler ClusterVersionHandler) GetStatus() (models.OperatorStatus, string, string, error) {
	co, err := handler.kc.GetClusterVersion()
	if err != nil {
		return "", "", "", err
	}
	handler.log.Infof("CVO status conditions: %+v", co.Status.Conditions)

	operatorStatus, operatorMessage := utils.MonitoredOperatorStatus(co.Status.Conditions)
	return operatorStatus, operatorMessage, co.Status.Desired.Version, nil
}

func (handler ClusterVersionHandler) OnChange(_ models.OperatorStatus) bool {
	return true
}

type ClusterServiceVersionHandler struct {
	kc       k8s_client.K8SClient
	operator *models.MonitoredOperator
	status   *ControllerStatus
	retries  int
	log      *logrus.Logger
}

func NewClusterServiceVersionHandler(log *logrus.Logger, kc k8s_client.K8SClient, operator *models.MonitoredOperator, status *ControllerStatus) *ClusterServiceVersionHandler {
	return &ClusterServiceVersionHandler{kc: kc, operator: operator, status: status, retries: 0, log: log}
}

func (handler ClusterServiceVersionHandler) GetName() string { return handler.operator.Name }

func (handler ClusterServiceVersionHandler) IsInitialized() bool {
	err := handler.handleOLMEarlySetupBug()
	if err != nil {
		handler.log.WithError(err).Warnf("Failed to handle OLMEarlySetupBug")
	}

	csvName, err := handler.kc.GetCSVFromSubscription(handler.operator.Namespace, handler.operator.SubscriptionName)
	if err != nil {
		return false
	}
	handler.log.Infof("Operator %s csv name is %s", handler.GetName(), csvName)
	if _, err := handler.kc.GetCSV(handler.operator.Namespace, csvName); err != nil {
		if apierrors.IsNotFound(err) {
			handler.log.WithError(err).Warnf("CSV %s does not exist for %s. ", csvName, handler.GetName())
		} else {
			handler.log.WithError(err).Warnf("Failed to get csv %s not for %s. ", csvName, handler.GetName())
		}

		return false
	}

	return true
}

// handleOLMEarlySetupBug cleans failed olm jobs as they will rerun by olm and
// deletes install plans to force olm re-conciliation and job recreation.
// Assisted controller does this because subscriptions created too early in the installation
// process might fail operator installation, and OLM will not retry installing
// them.
func (handler ClusterServiceVersionHandler) handleOLMEarlySetupBug() error {
	handler.log.Infof("Check if there are failed olm jobs and delete them in case they exists")
	err := handler.deleteFailedOlmJobs()
	if err != nil {
		handler.log.WithError(err).Warnf("Failed to delete olm jobs")
		return err
	}
	// OLM for some reason doesn't reconcile the subscription if job was deleted
	// so we have to delete failed install plans to force OLM
	// to re-create them and jobs that were deleted previously
	err = handler.deleteFailedSubscriptionInstallPlans()
	if err != nil {
		handler.log.WithError(err).Warnf("Failed to delete %s install plan", handler.GetName())
		return err
	}

	return nil
}

func (handler ClusterServiceVersionHandler) deleteFailedOlmJobs() error {
	jobs, err := handler.kc.ListJobs(olmNamespace)
	if err != nil {
		return err
	}
	for _, job := range jobs.Items {
		handler.log.Debugf("Checking job %s", job.Name)
		if jobFailed(job) {
			handler.log.Infof("Found failed olm job %s, deleting it", job.Name)
			if err := handler.kc.DeleteJob(types.NamespacedName{
				Name:      job.Name,
				Namespace: job.Namespace,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func jobFailed(job batchV1.Job) bool {
	if job.Status.Failed > 0 {
		return true
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchV1.JobFailed {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

func (handler ClusterServiceVersionHandler) deleteFailedSubscriptionInstallPlans() error {
	subscriptionInstallPlans, err := handler.kc.GetAllInstallPlansOfSubscription(types.NamespacedName{
		Name:      handler.operator.SubscriptionName,
		Namespace: handler.operator.Namespace,
	})
	if err != nil {
		return err
	}

	for _, installPlan := range subscriptionInstallPlans {
		if installPlan.Status.Phase != olmv1alpha1.InstallPlanPhaseFailed {
			continue
		}
		handler.log.Infof("Deleting failed install plan %s", installPlan.Name)
		if err := handler.kc.DeleteInstallPlan(types.NamespacedName{
			Name:      installPlan.Name,
			Namespace: installPlan.Namespace,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (handler ClusterServiceVersionHandler) GetStatus() (models.OperatorStatus, string, string, error) {
	csvName, err := handler.kc.GetCSVFromSubscription(handler.operator.Namespace, handler.operator.SubscriptionName)
	if err != nil {
		return "", "", "", err
	}
	csv, err := handler.kc.GetCSV(handler.operator.Namespace, csvName)
	if err != nil {
		return "", "", "", err
	}
	operatorStatus := utils.CsvStatusToOperatorStatus(string(csv.Status.Phase))
	return operatorStatus, csv.Status.Message, csv.Spec.Version.String(), nil
}

func (handler ClusterServiceVersionHandler) OnChange(newStatus models.OperatorStatus) bool {
	if IsStatusFailed(newStatus) {
		if handler.retries < failedOperatorRetry {
			// FIXME: We retry the check of the operator status in case it's in failed state to WA bug 1968606
			// Remove this code when bug 1968606 is fixed
			handler.retries++ //nolint:staticcheck
			return false
		}
		handler.status.OperatorError(handler.operator.Name)
	}

	return true
}

func IsStatusFailed(operatorStatus models.OperatorStatus) bool {
	return operatorStatus == models.OperatorStatusFailed
}

func IsStatusSucceeded(operatorStatus models.OperatorStatus) bool {
	return operatorStatus == models.OperatorStatusAvailable
}
