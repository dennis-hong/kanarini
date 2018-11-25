package controller

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"
	kanarini "github.com/nilebox/kanarini/pkg/apis/kanarini/v1alpha1"
	"github.com/nilebox/kanarini/pkg/kubernetes/pkg/controller"
	"github.com/pkg/errors"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

// rolloutCanary implements the logic for a canary deployment.
func (c *CanaryDeploymentController) rolloutCanary(cd *kanarini.CanaryDeployment, dList []*apps.Deployment) error {
	cdBytes, _ := json.Marshal(cd)
	glog.V(4).Infof("CanaryDeployment: %v", string(cdBytes))

	// Last seen template
	template := &cd.Spec.Template
	templateHash := controller.ComputeHash(template, nil)
	cd.Status.ObservedGeneration = cd.Generation
	cd.Status.ObservedTemplateHash = templateHash

	// Check if we need to rollback
	rollbackTemplate, err := getRollbackTemplate(cd, templateHash)
	if err != nil {
		return err
	}
	if rollbackTemplate != nil {
		glog.V(4).Info("Rolling back to the latest successful template")
		c.eventRecorder.Event(cd, corev1.EventTypeNormal, RollingBackReason, RollingBackMessage)
		// Ignore spec template since it's broken
		template = rollbackTemplate
		templateHash = controller.ComputeHash(template, nil)
	}

	// Create a canary track deployment
	canaryTrackDeployment, err := c.createTrackDeployment(cd, template, templateHash, dList, &cd.Spec.Tracks.Canary.TrackDeploymentSpec, kanarini.CanaryTrackName)
	if err != nil {
		c.eventRecorder.Event(cd, corev1.EventTypeWarning, FailedToCreateCanaryTrackDeploymentReason, FailedToCreateCanaryTrackDeploymentMessage)
		return err
	}
	// Wait for a canary track deployment to succeed
	if !IsDeploymentReady(canaryTrackDeployment) {
		glog.V(4).Info("Canary track deployment is not ready")
		c.eventRecorder.Event(cd, corev1.EventTypeNormal, CanaryTrackDeploymentNotReadyReason, CanaryTrackDeploymentNotReadyMessage)
		// We will get an event once Deployment object is updated
		return nil
	}
	glog.V(4).Info("Canary track deployment is ready!")
	c.eventRecorder.Event(cd, corev1.EventTypeNormal, CanaryTrackDeploymentReadyReason, CanaryTrackDeploymentReadyMessage)
	// If the template was already successfully checked before, skip metrics delay and check
	if cd.Status.LatestSuccessfulDeploymentSnapshot == nil || cd.Status.LatestSuccessfulDeploymentSnapshot.TemplateHash != templateHash {
		// Wait for metric delay to expire
		metricCheckDelay := time.Duration(cd.Spec.Tracks.Canary.MetricsCheckDelaySeconds) * time.Second
		if cd.Status.CanaryDeploymentReadyStatusCheckpoint == nil || templateHash != cd.Status.CanaryDeploymentReadyStatusCheckpoint.TemplateHash {
			glog.V(4).Info("Recording a ready status checkpoint")
			cd.Status.CanaryDeploymentReadyStatusCheckpoint = &kanarini.DeploymentReadyStatusCheckpoint{
				TemplateHash:         templateHash,
				LatestReadyTimestamp: metav1.Now(),
			}
			cd, err = c.kanariniClient.CanaryDeployments(cd.Namespace).UpdateStatus(cd)
			if err != nil {
				glog.V(4).Infof("Failed to update CanaryDeployment status: %v", err)
				return err
			}
			// Delay re-processing of deployment by configured delay
			glog.V(4).Infof("Delay re-processing of deployment by configured delay: %v", metricCheckDelay)
			c.eventRecorder.Eventf(cd, corev1.EventTypeNormal, DelayMetricsCheckReason, "Delay metrics check by configured delay: %v", metricCheckDelay)
			c.enqueueAfter(cd, metricCheckDelay)
			return nil
		}
		checkpoint := cd.Status.CanaryDeploymentReadyStatusCheckpoint
		if checkpoint.MetricCheckResult == kanarini.MetricCheckResultUnknown {
			metricCheckTime := checkpoint.LatestReadyTimestamp.Add(metricCheckDelay)
			remainingDelay := metricCheckTime.Sub(time.Now())
			if remainingDelay > 0 {
				// Delay re-processing of deployment by remaining delay
				glog.V(4).Infof("Delay re-processing of deployment by remaining delay: %v", remainingDelay)
				c.eventRecorder.Eventf(cd, corev1.EventTypeNormal, DelayMetricsCheckReason, "Delay metrics check by remaining delay: %v", remainingDelay)
				c.enqueueAfter(cd, remainingDelay)
				return nil
			}
			// Check the metric value and decide whether Service is healthy
			result, err := c.checkDeploymentMetric(cd, &cd.Spec.Tracks.Canary)
			if err != nil {
				return err
			}
			glog.V(4).Infof("Metric check result: %q", result)
			c.eventRecorder.Eventf(cd, corev1.EventTypeNormal, MetricsCheckResultReason, "Metrics check result: %v", result)

			checkpoint.MetricCheckResult = result
			templateBytes, err := json.Marshal(cd.Spec.Template)
			if err != nil {
				glog.V(4).Info("Failed to marshal template")
				return err
			}
			if result == kanarini.MetricCheckResultSuccess {
				cd.Status.LatestSuccessfulDeploymentSnapshot = &kanarini.DeploymentSnapshot{
					TemplateHash: templateHash,
					Template:     string(templateBytes),
					Timestamp:    metav1.Now(),
				}
			} else if result == kanarini.MetricCheckResultFailure {
				cd.Status.LatestFailedDeploymentSnapshot = &kanarini.DeploymentSnapshot{
					TemplateHash: templateHash,
					Template:     string(templateBytes),
					Timestamp:    metav1.Now(),
				}
			}
			cd, err = c.kanariniClient.CanaryDeployments(cd.Namespace).UpdateStatus(cd)
			if err != nil {
				return err
			}
			// We will get an event with up-to-date object
			return nil
		}
		if checkpoint.MetricCheckResult != kanarini.MetricCheckResultSuccess {
			glog.V(4).Info("Canary track deployment is not healthy. Stopping propagation")
			c.eventRecorder.Event(cd, corev1.EventTypeWarning, MetricsCheckUnsuccessfulReason, "Metrics check is unsuccessful, stopping propagation")
			return nil
		}
	}
	// Create a stable track deployment
	stableTrackDeployment, err := c.createTrackDeployment(cd, template, templateHash, dList, &cd.Spec.Tracks.Stable, kanarini.StableTrackName)
	// Wait for a canary track deployment to succeed
	if !IsDeploymentReady(stableTrackDeployment) {
		glog.V(4).Info("Stable track deployment is not ready")
		c.eventRecorder.Event(cd, corev1.EventTypeNormal, StableTrackDeploymentNotReadyReason, StableTrackDeploymentNotReadyMessage)
		// We will get an event once Deployment object is updated
		return nil
	}
	glog.V(4).Info("Stable track deployment is ready!")
	c.eventRecorder.Event(cd, corev1.EventTypeNormal, StableTrackDeploymentReadyReason, StableTrackDeploymentReadyMessage)
	// Done
	glog.V(4).Infof("Finished reconciling canary deployment %s/%s", cd.Namespace, cd.Name)
	c.eventRecorder.Event(cd, corev1.EventTypeNormal, DoneProcessingReason, DoneProcessingMessage)
	return nil
}

func (c *CanaryDeploymentController) checkDeploymentMetric(cd *kanarini.CanaryDeployment, trackSpec *kanarini.CanaryTrackDeploymentSpec) (kanarini.MetricCheckResult, error) {
	for _, metricSpec := range trackSpec.Metrics {
		switch metricSpec.Type {
		case kanarini.ObjectMetricSourceType:
			metricSelector, err := metav1.LabelSelectorAsSelector(metricSpec.Object.Metric.Selector)
			if err != nil {
				return "", err
			}
			val, _, err := c.metricsClient.GetObjectMetric(metricSpec.Object.Metric.Name, cd.Namespace, &metricSpec.Object.DescribedObject, metricSelector)
			if err != nil {
				return "", err
			}
			glog.V(4).Infof("Custom metric value: %v", val)
			glog.V(4).Infof("Custom metric target value: %v", metricSpec.Object.Target.Value.MilliValue())
			// Sometimes we get "value": "-9223372036854775808m" in Response from Prometheus Adapter,
			// Either it's a bug in Prometheus Adapter, or some behavior of Prometheus that we don't understand
			// For now force retry loop in that case
			if val < 0 {
				return kanarini.MetricCheckResultUnknown, errors.New("Negative metric values are not supported, the check will be retried")
			}
			// If metric value is equal or greater than target value, it's considered unhealthy
			if val >= metricSpec.Object.Target.Value.MilliValue() {
				return kanarini.MetricCheckResultFailure, nil
			}
		case kanarini.ExternalMetricSourceType:
			metricSelector, err := metav1.LabelSelectorAsSelector(metricSpec.External.Metric.Selector)
			if err != nil {
				return "", err
			}
			metrics, _, err := c.metricsClient.GetExternalMetric(metricSpec.Object.Metric.Name, cd.Namespace, metricSelector)
			if err != nil {
				return "", err
			}
			var sum int64 = 0
			for _, val := range metrics {
				sum = sum + val
			}
			// If metric value is equal or greater than target value, it's considered unhealthy
			if sum >= metricSpec.External.Target.Value.MilliValue() {
				return kanarini.MetricCheckResultFailure, nil
			}
		default:
			return "", errors.New(fmt.Sprintf("Unexpected metric source type: %v", metricSpec.Type))
		}
	}

	return kanarini.MetricCheckResultSuccess, nil
}

func (c *CanaryDeploymentController) createTrackDeployment(cd *kanarini.CanaryDeployment, template *corev1.PodTemplateSpec, templateHash string, dList []*apps.Deployment, trackSpec *kanarini.TrackDeploymentSpec, trackName kanarini.CanaryDeploymentTrackName) (*apps.Deployment, error) {
	template = template.DeepCopy()
	annotations := template.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[kanarini.TemplateHashAnnotationKey] = templateHash
	labels := template.Labels
	if labels == nil {
		labels = make(map[string]string)
	}
	for k, v := range trackSpec.Labels {
		labels[k] = v
	}
	newDeployment := apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			// Make the name deterministic, to ensure idempotence
			Name:            cd.Name + "-" + string(trackName),
			Namespace:       cd.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cd, kanarini.CanaryDeploymentGVK)},
			Annotations: annotations,
			Labels:          labels,
		},
		Spec: apps.DeploymentSpec{
			Template:                *template,
			Replicas:                trackSpec.Replicas,
			Selector:                cd.Spec.Selector,
			MinReadySeconds:         cd.Spec.MinReadySeconds,
			ProgressDeadlineSeconds: cd.Spec.ProgressDeadlineSeconds,
		},
	}
	// TODO this means we ignore selector from CD spec, we should extend the selector separately instead
	newDeployment.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: template.Labels,
	}

	// Create the new Deployment. If it already exists, then we need to check for possible
	// conflicts. If there is any other error, we need to report it in the status of
	// the CanaryDeployment.
	alreadyExists := false
	createdDeployment, err := c.kubeClient.AppsV1().Deployments(cd.Namespace).Create(&newDeployment)
	switch {
	// We may end up hitting this due to a slow cache or a fast resync of the Deployment.
	case apierrors.IsAlreadyExists(err):
		alreadyExists = true

		// Fetch a copy of the Deployment.
		d, dErr := c.dLister.Deployments(newDeployment.Namespace).Get(newDeployment.Name)
		if dErr != nil {
			return nil, dErr
		}

		controllerRef := metav1.GetControllerOf(d)
		if controllerRef != nil && controllerRef.UID == cd.UID {
			createdDeployment = d
			err = nil
			// TODO: also need to check contents to make sure that there were no manual changes to deployment
			if createdDeployment.Annotations[kanarini.TemplateHashAnnotationKey] != newDeployment.Annotations[kanarini.TemplateHashAnnotationKey] {
				// Pod template hashes are different; need to update the deployment
				createdDeployment = createdDeployment.DeepCopy()
				createdDeployment.Annotations = newDeployment.Annotations
				createdDeployment.Labels = newDeployment.Labels
				createdDeployment.Spec = newDeployment.Spec
				createdDeployment, err = c.kubeClient.AppsV1().Deployments(createdDeployment.Namespace).Update(createdDeployment)
				if err != nil {
					return nil, err
				}
			}
			break
		}

		msg := fmt.Sprintf("New Deployment conflicts with existing one: %q", newDeployment.Name)
		if HasProgressDeadline(cd) {
			cond := NewCanaryDeploymentCondition(kanarini.CanaryDeploymentProgressing, corev1.ConditionFalse, FailedDeploymentCreateReason, msg)
			SetCanaryDeploymentCondition(&cd.Status, *cond)
			// We don't really care about this error at this point, since we have a bigger issue to report.
			_, _ = c.kanariniClient.CanaryDeployments(cd.Namespace).UpdateStatus(cd)
		}
		c.eventRecorder.Eventf(cd, corev1.EventTypeWarning, FailedDeploymentCreateReason, msg)
		return nil, fmt.Errorf("new Deployment conflicts with existing one: %q", newDeployment.Name)
	case err != nil:
		msg := fmt.Sprintf("Failed to create new Deployment %q: %v", newDeployment.Name, err)
		if HasProgressDeadline(cd) {
			cond := NewCanaryDeploymentCondition(kanarini.CanaryDeploymentProgressing, corev1.ConditionFalse, FailedDeploymentCreateReason, msg)
			SetCanaryDeploymentCondition(&cd.Status, *cond)
			// We don't really care about this error at this point, since we have a bigger issue to report.
			_, _ = c.kanariniClient.CanaryDeployments(cd.Namespace).UpdateStatus(cd)
		}
		c.eventRecorder.Eventf(cd, corev1.EventTypeWarning, FailedDeploymentCreateReason, msg)
		return nil, err
	}

	needsUpdate := false
	if !alreadyExists && HasProgressDeadline(cd) {
		msg := fmt.Sprintf("Created new Deployment %q", createdDeployment.Name)
		condition := NewCanaryDeploymentCondition(kanarini.CanaryDeploymentProgressing, corev1.ConditionTrue, NewDeploymentReason, msg)
		SetCanaryDeploymentCondition(&cd.Status, *condition)
		needsUpdate = true
	}
	if needsUpdate {
		cd, err = c.kanariniClient.CanaryDeployments(cd.Namespace).UpdateStatus(cd)
		if err != nil {
			return nil, err
		}
		return createdDeployment, nil
	}
	return createdDeployment, nil
}

func getRollbackTemplate(cd *kanarini.CanaryDeployment, templateHash string) (*corev1.PodTemplateSpec, error) {
	if cd.Status.LatestFailedDeploymentSnapshot != nil && cd.Status.LatestFailedDeploymentSnapshot.TemplateHash == templateHash {
		// Rollback to the latest successful deployment
		if cd.Status.LatestSuccessfulDeploymentSnapshot != nil {
			var template corev1.PodTemplateSpec
			err := json.Unmarshal([]byte(cd.Status.LatestSuccessfulDeploymentSnapshot.Template), &template)
			if err != nil {
				return nil, err
			}
			return &template, nil
		}
	}

	return nil, nil
}
