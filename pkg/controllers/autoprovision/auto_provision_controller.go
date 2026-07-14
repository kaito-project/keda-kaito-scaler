// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package autoprovision

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"golang.org/x/time/rate"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scaledobject"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/inferenceset"
)

const (
	requeueInterval = 5 * time.Second

	// annotationPrefix is the common prefix for every autoscaling annotation the
	// controller consumes on an InferenceSet. Any change to a key under this
	// prefix (including the indexed composite keys) triggers a reconcile.
	annotationPrefix = "scaledobject.kaito.sh/"
)

type Controller struct {
	client.Client
	ScalerNamespace   string
	ScalerServiceName string
	ScalerGRPCPort    int
	Recorder          record.EventRecorder
}

func NewAutoProvisionController(c client.Client, scalerNamespace, scalerServiceName string, scalerGRPCPort int, recorder record.EventRecorder) *Controller {
	return &Controller{
		Client:            c,
		ScalerNamespace:   scalerNamespace,
		ScalerServiceName: scalerServiceName,
		ScalerGRPCPort:    scalerGRPCPort,
		Recorder:          recorder,
	}
}

func (c *Controller) Reconcile(ctx context.Context, is *kaitov1beta1.InferenceSet) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("auto-provision-controller")

	// When auto-provisioning is not requested (annotation missing or set to a
	// value other than "true"), tear down any ScaledObject we previously
	// provisioned for this InferenceSet. This handles the case where the
	// auto-provision annotation is toggled off after a ScaledObject was created.
	if !autoProvisionRequested(is) {
		return reconcile.Result{}, c.cleanupManagedScaledObjects(ctx, is)
	}

	if !autoscalingConfigValid(is) {
		return reconcile.Result{}, nil
	}

	maxReplicas, requeue, err := c.resolveMaxReplicas(ctx, is)
	if err != nil {
		return reconcile.Result{}, err
	}
	if requeue {
		logger.Info("target node count from workspaces for inference set is zero, requeuing",
			"namespace", is.Namespace, "name", is.Name, "requeueAfter", requeueInterval.String())
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	minReplicas := resolveMinReplicas(is.Annotations)
	if maxReplicas < minReplicas {
		logger.Info("skip reconciling inference set because max-replicas is less than min-replicas",
			"namespace", is.Namespace, "name", is.Name,
			"minReplicas", minReplicas, "maxReplicas", maxReplicas)
		if c.Recorder != nil {
			c.Recorder.Eventf(is, corev1.EventTypeWarning, "InvalidReplicaRange",
				"Skip auto-provisioning ScaledObject: max-replicas (%d) is less than min-replicas (%d)",
				maxReplicas, minReplicas)
		}
		return reconcile.Result{}, nil
	}

	managedScaledObjects, err := c.listManagedScaledObjects(ctx, is)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Build the desired ScaledObject from the InferenceSet's auto-provision
	// annotations (one or more indexed metrics combined under a conservative AND
	// policy).
	soBuilder := scaledobject.Builder{
		ScalerNamespace:   c.ScalerNamespace,
		ScalerServiceName: c.ScalerServiceName,
		ScalerGRPCPort:    c.ScalerGRPCPort,
	}
	desired, err := soBuilder.BuildDesired(is, minReplicas, maxReplicas)
	if err != nil {
		logger.Info("skip reconciling inference set because autoscaling config is invalid",
			"namespace", is.Namespace, "name", is.Name, "error", err.Error())
		if c.Recorder != nil {
			c.Recorder.Eventf(is, corev1.EventTypeWarning, "InvalidConfig",
				"Skip auto-provisioning ScaledObject: %v", err)
		}
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, c.syncDesired(ctx, managedScaledObjects, desired)
}

// resolveMaxReplicas computes the max replicas for the ScaledObject. When
// NodeCountLimit is set, it derives the value from workspaces and may signal a
// requeue if information is not yet available. Otherwise it falls back to the
// max-replicas annotation or a default of 1.
func (c *Controller) resolveMaxReplicas(ctx context.Context, is *kaitov1beta1.InferenceSet) (int, bool, error) {
	if is.Spec.NodeCountLimit == 0 {
		if maxReplicasStr, ok := is.Annotations[constants.AnnotationKeyMaxReplicas]; ok {
			v, err := strconv.Atoi(maxReplicasStr)
			if err != nil || v < 1 {
				v = 1
			} else if v > math.MaxInt32 {
				v = math.MaxInt32
			}
			return v, false, nil
		}
		return 1, false, nil
	}

	workspaceList, err := inferenceset.ListWorkspaces(ctx, is, c.Client)
	if err != nil {
		return 0, false, fmt.Errorf("failed to list workspaces for inference set %s/%s: %v", is.Namespace, is.Name, err)
	}

	var targetNodeCount int32
	for i := range workspaceList.Items {
		if workspaceList.Items[i].Status.TargetNodeCount != 0 {
			targetNodeCount = workspaceList.Items[i].Status.TargetNodeCount
			break
		}
	}
	if targetNodeCount == 0 {
		return 0, true, nil
	}

	maxReplicas := is.Spec.NodeCountLimit / int(targetNodeCount)
	if maxReplicas < 1 {
		maxReplicas = 1
	} else if maxReplicas > math.MaxInt32 {
		maxReplicas = math.MaxInt32
	}
	return maxReplicas, false, nil
}

// listManagedScaledObjects returns ScaledObjects that target the given
// InferenceSet and are managed by this scaler.
func (c *Controller) listManagedScaledObjects(ctx context.Context, is *kaitov1beta1.InferenceSet) ([]v1alpha1.ScaledObject, error) {
	var scaledObjectList v1alpha1.ScaledObjectList
	if err := c.List(ctx, &scaledObjectList, client.InNamespace(is.Namespace)); err != nil {
		return nil, err
	}

	var managed []v1alpha1.ScaledObject
	for i := range scaledObjectList.Items {
		so := scaledObjectList.Items[i]
		// Only ScaledObjects owned (controller reference) by this InferenceSet are
		// considered managed. This is stricter and more robust than matching on the
		// scale target and managed-by annotation: it excludes ScaledObjects a user
		// created by hand for the same InferenceSet, which we must not touch.
		owner := metav1.GetControllerOf(&so)
		if owner != nil && owner.UID == is.UID && owner.Kind == constants.InferenceSet {
			managed = append(managed, so)
		}
	}
	return managed, nil
}

// cleanupManagedScaledObjects deletes every ScaledObject this controller
// provisioned for the InferenceSet. It is invoked when auto-provisioning is
// disabled so scaling is fully torn down. Missing objects are treated as
// already cleaned up.
func (c *Controller) cleanupManagedScaledObjects(ctx context.Context, is *kaitov1beta1.InferenceSet) error {
	managed, err := c.listManagedScaledObjects(ctx, is)
	if err != nil {
		return err
	}
	for i := range managed {
		if managed[i].DeletionTimestamp != nil {
			continue
		}
		if err := c.Delete(ctx, &managed[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if c.Recorder != nil {
			c.Recorder.Eventf(is, corev1.EventTypeNormal, "ScaledObjectCleanedUp",
				"Deleted auto-provisioned ScaledObject %s because auto-provisioning of InferenceSet(%s/%s) is disabled", managed[i].Name, is.Namespace, is.Name)
		}
	}
	return nil
}

// syncDesired creates, updates, or deduplicates managed ScaledObjects so that
// exactly one matches the desired spec (single-metric or composite).
func (c *Controller) syncDesired(ctx context.Context, existing []v1alpha1.ScaledObject, desired *v1alpha1.ScaledObject) error {
	switch len(existing) {
	case 0:
		return c.Create(ctx, desired)
	case 1:
		return c.reconcileTo(ctx, &existing[0], desired)
	default:
		// Keep the oldest one, delete the rest, then reconcile the kept one to
		// the desired spec so it does not go stale after annotation changes.
		sort.SliceStable(existing, func(i, j int) bool {
			return existing[i].CreationTimestamp.Before(&existing[j].CreationTimestamp)
		})
		for i := 1; i < len(existing); i++ {
			if err := c.Delete(ctx, &existing[i]); err != nil {
				return err
			}
		}
		return c.reconcileTo(ctx, &existing[0], desired)
	}
}

// reconcileTo mutates the existing ScaledObject toward the desired spec,
// updating only the fields this controller manages (replica bounds, polling
// interval, triggers, and the advanced HPA/scalingModifiers config) and issues
// an Update only when something actually changed.
func (c *Controller) reconcileTo(ctx context.Context, existing, desired *v1alpha1.ScaledObject) error {
	updated := false

	if !equalInt32Ptr(existing.Spec.MinReplicaCount, desired.Spec.MinReplicaCount) {
		existing.Spec.MinReplicaCount = desired.Spec.MinReplicaCount
		updated = true
	}
	if !equalInt32Ptr(existing.Spec.MaxReplicaCount, desired.Spec.MaxReplicaCount) {
		existing.Spec.MaxReplicaCount = desired.Spec.MaxReplicaCount
		updated = true
	}
	if !equalInt32Ptr(existing.Spec.PollingInterval, desired.Spec.PollingInterval) {
		existing.Spec.PollingInterval = desired.Spec.PollingInterval
		updated = true
	}
	if !reflect.DeepEqual(existing.Spec.Triggers, desired.Spec.Triggers) {
		existing.Spec.Triggers = desired.Spec.Triggers
		updated = true
	}
	if advancedChanged(existing.Spec.Advanced, desired.Spec.Advanced) {
		existing.Spec.Advanced = desired.Spec.Advanced
		updated = true
	}

	if !updated {
		return nil
	}
	return c.Update(ctx, existing)
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// advancedChanged reports whether the managed subset of AdvancedConfig differs
// between the current and desired ScaledObject. It compares scalingModifiers by
// value and the HPA behaviour by the specific fields the controller sets,
// avoiding reflect.DeepEqual on resource.Quantity (whose cached string field can
// cause spurious diffs and update loops).
func advancedChanged(existing, desired *v1alpha1.AdvancedConfig) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}
	if existing.ScalingModifiers != desired.ScalingModifiers {
		return true
	}
	return hpaConfigChanged(existing.HorizontalPodAutoscalerConfig, desired.HorizontalPodAutoscalerConfig)
}

func hpaConfigChanged(existing, desired *v1alpha1.HorizontalPodAutoscalerConfig) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}
	if (existing.Behavior == nil) != (desired.Behavior == nil) {
		return true
	}
	if existing.Behavior == nil {
		return false
	}
	return scalingRulesChanged(existing.Behavior.ScaleUp, desired.Behavior.ScaleUp) ||
		scalingRulesChanged(existing.Behavior.ScaleDown, desired.Behavior.ScaleDown)
}

func scalingRulesChanged(existing, desired *autoscalingv2.HPAScalingRules) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}
	if !equalInt32Ptr(existing.StabilizationWindowSeconds, desired.StabilizationWindowSeconds) {
		return true
	}
	if quantityString(existing.Tolerance) != quantityString(desired.Tolerance) {
		return true
	}
	if len(existing.Policies) != len(desired.Policies) {
		return true
	}
	for i := range existing.Policies {
		if existing.Policies[i].Type != desired.Policies[i].Type ||
			existing.Policies[i].Value != desired.Policies[i].Value ||
			existing.Policies[i].PeriodSeconds != desired.Policies[i].PeriodSeconds {
			return true
		}
	}
	return false
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

func generateInferenceSetPredicateFunc() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			inferenceSet, ok := e.Object.(*kaitov1beta1.InferenceSet)
			if !ok {
				return false
			}
			return autoProvisionRequested(inferenceSet) && autoscalingConfigValid(inferenceSet)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldInferenceSet, ok := e.ObjectOld.(*kaitov1beta1.InferenceSet)
			if !ok {
				return false
			}

			newInferenceSet, ok := e.ObjectNew.(*kaitov1beta1.InferenceSet)
			if !ok {
				return false
			}

			// Reconcile when any autoscaling annotation changes. Comparing by
			// prefix covers the fixed single-metric keys as well as the indexed
			// composite keys (metricName/0, upthreshold/0, ...).
			if annotationsWithPrefixChanged(oldInferenceSet.Annotations, newInferenceSet.Annotations, annotationPrefix) {
				// Fire when the new state is validly enabled (provision/update) or
				// the old state requested auto-provisioning (so toggling the
				// annotation off still triggers cleanup of the ScaledObject).
				return (autoProvisionRequested(newInferenceSet) && autoscalingConfigValid(newInferenceSet)) || autoProvisionRequested(oldInferenceSet)
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

func resolveMinReplicas(annotations map[string]string) int {
	if minReplicasStr, ok := annotations[constants.AnnotationKeyMinReplicas]; ok {
		if v, err := strconv.Atoi(minReplicasStr); err == nil && v > 1 {
			if v > math.MaxInt32 {
				return math.MaxInt32
			}
			return v
		}
	}
	return 1
}

// autoscalingConfigValid reports whether the InferenceSet's autoscaling
// configuration is valid enough to provision a ScaledObject. It assumes the
// caller has already confirmed auto-provisioning was requested (see
// autoProvisionRequested) and only validates the replica bounds and the
// auto-provision configuration.
func autoscalingConfigValid(inferenceSet *kaitov1beta1.InferenceSet) bool {
	// if max-replicas annotation exists, max replicas should be more than 1
	// if not exists, NodeCountLimit should be more than 1, we will use it to calculate max replicas
	if maxReplicasStr, ok := inferenceSet.Annotations[constants.AnnotationKeyMaxReplicas]; ok {
		if maxReplicas, err := strconv.Atoi(maxReplicasStr); err != nil || maxReplicas <= 1 {
			return false
		}
	} else if inferenceSet.Spec.NodeCountLimit == 0 {
		return false
	}

	return scaledobject.ValidateConfig(inferenceSet.Annotations) == nil
}

// autoProvisionRequested reports whether the InferenceSet opts into
// auto-provisioning via the scaledobject.kaito.sh/auto-provision annotation.
func autoProvisionRequested(inferenceSet *kaitov1beta1.InferenceSet) bool {
	return inferenceSet.Annotations[constants.AnnotationKeyAutoProvision] == "true"
}

// annotationsWithPrefixChanged reports whether any annotation key starting with
// prefix differs between the old and new maps (including keys added or removed).
func annotationsWithPrefixChanged(oldAnnotations, newAnnotations map[string]string, prefix string) bool {
	for k, v := range oldAnnotations {
		if strings.HasPrefix(k, prefix) && newAnnotations[k] != v {
			return true
		}
	}
	for k, v := range newAnnotations {
		if strings.HasPrefix(k, prefix) && oldAnnotations[k] != v {
			return true
		}
	}
	return false
}

// +kubebuilder:rbac:groups="keda.sh",resources=scaledobjects,verbs=create;list;watch;get;update;delete
// +kubebuilder:rbac:groups="kaito.sh",resources=inferencesets,verbs=list;watch;get
// +kubebuilder:rbac:groups="kaito.sh",resources=workspaces,verbs=list;watch

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("scaledobject.provision").
		For(&kaitov1beta1.InferenceSet{}, builder.WithPredicates(generateInferenceSetPredicateFunc())).
		Watches(&v1alpha1.ScaledObject{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			if scaledObj, ok := obj.(*v1alpha1.ScaledObject); ok {
				if scaledObj.Spec.ScaleTargetRef.Kind == constants.InferenceSet {
					return []reconcile.Request{
						{
							NamespacedName: types.NamespacedName{Namespace: scaledObj.Namespace, Name: scaledObj.Spec.ScaleTargetRef.Name},
						},
					}
				}
			}
			return []reconcile.Request{}
		})).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, 300*time.Second),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
