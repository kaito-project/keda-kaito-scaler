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
	"sort"
	"strconv"
	"time"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"golang.org/x/time/rate"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
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

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/inferenceset"
)

const (
	AnnotationKeyAutoProvision = "scaledobject.kaito.sh/auto-provision"
	AnnotationKeyMinReplicas   = "scaledobject.kaito.sh/min-replicas"
	AnnotationKeyMaxReplicas   = "scaledobject.kaito.sh/max-replicas"
	AnnotationKeyThreshold     = "scaledobject.kaito.sh/threshold"
	AnnotationKeyMetricName    = "scaledobject.kaito.sh/metricName"

	AnnotationKeyManagedBy = "scaledobject.kaito.sh/managed-by"
	InferenceSet           = "InferenceSet"

	inferenceSetAPIVersion = "kaito.sh/v1alpha1"
	requeueInterval        = 5 * time.Second

	// defaultMetricName is the metric scraped from each vLLM inference pod when
	// the user does not override it via the metricName annotation.
	defaultMetricName = "vllm:num_requests_waiting"

	// defaultPollingInterval is the interval (in seconds) at which KEDA polls
	// the external scaler for metrics. Overrides KEDA's built-in default of 30s
	// to give the autoscaler fresher signals for latency-sensitive inference
	// workloads.
	defaultPollingInterval = 15
)

// watchedAnnotations are the annotations whose changes should trigger a
// reconcile of the owning InferenceSet.
var watchedAnnotations = []string{
	AnnotationKeyAutoProvision,
	AnnotationKeyMinReplicas,
	AnnotationKeyMaxReplicas,
	AnnotationKeyThreshold,
	AnnotationKeyMetricName,
}

type Controller struct {
	client.Client
	ScalerNamespace string
	Recorder        record.EventRecorder
}

func NewAutoProvisionController(c client.Client, scalerNamespace string, recorder record.EventRecorder) *Controller {
	return &Controller{
		Client:          c,
		ScalerNamespace: scalerNamespace,
		Recorder:        recorder,
	}
}

func (c *Controller) Reconcile(ctx context.Context, is *kaitov1alpha1.InferenceSet) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithName("auto-provision-controller")
	if !enableAutoProvisioning(is) {
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
	threshold := is.Annotations[AnnotationKeyThreshold]
	metricName := resolveMetricName(is.Annotations)

	managedScaledObjects, err := c.listManagedScaledObjects(ctx, is)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, c.syncScaledObjects(ctx, is, managedScaledObjects, minReplicas, maxReplicas, threshold, metricName)
}

// resolveMaxReplicas computes the max replicas for the ScaledObject. When
// NodeCountLimit is set, it derives the value from workspaces and may signal a
// requeue if information is not yet available. Otherwise it falls back to the
// max-replicas annotation or a default of 1.
func (c *Controller) resolveMaxReplicas(ctx context.Context, is *kaitov1alpha1.InferenceSet) (int, bool, error) {
	if is.Spec.NodeCountLimit == 0 {
		if maxReplicasStr, ok := is.Annotations[AnnotationKeyMaxReplicas]; ok {
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
func (c *Controller) listManagedScaledObjects(ctx context.Context, is *kaitov1alpha1.InferenceSet) ([]v1alpha1.ScaledObject, error) {
	var scaledObjectList v1alpha1.ScaledObjectList
	if err := c.List(ctx, &scaledObjectList, client.InNamespace(is.Namespace)); err != nil {
		return nil, err
	}

	var managed []v1alpha1.ScaledObject
	for _, so := range scaledObjectList.Items {
		if so.Spec.ScaleTargetRef.Name == is.Name &&
			so.Spec.ScaleTargetRef.Kind == InferenceSet &&
			so.Annotations[AnnotationKeyManagedBy] == scaler.ScalerName {
			managed = append(managed, so)
		}
	}
	return managed, nil
}

// syncScaledObjects creates, updates, or deduplicates managed ScaledObjects so
// that exactly one matches the desired spec.
func (c *Controller) syncScaledObjects(ctx context.Context, is *kaitov1alpha1.InferenceSet, existing []v1alpha1.ScaledObject, minReplicas, maxReplicas int, threshold, metricName string) error {
	switch len(existing) {
	case 0:
		return c.Create(ctx, c.buildScaledObject(is, minReplicas, maxReplicas, threshold, metricName))
	case 1:
		return c.updateScaledObject(ctx, is, &existing[0], minReplicas, maxReplicas, threshold, metricName)
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
		return c.updateScaledObject(ctx, is, &existing[0], minReplicas, maxReplicas, threshold, metricName)
	}
}

func (c *Controller) buildScaledObject(is *kaitov1alpha1.InferenceSet, minReplicas, maxReplicas int, threshold, metricName string) *v1alpha1.ScaledObject {
	return &v1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      is.Name,
			Namespace: is.Namespace,
			Annotations: map[string]string{
				AnnotationKeyManagedBy: scaler.ScalerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         inferenceSetAPIVersion,
					Kind:               InferenceSet,
					Name:               is.Name,
					UID:                is.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.ScaledObjectSpec{
			Advanced: &v1alpha1.AdvancedConfig{
				HorizontalPodAutoscalerConfig: getDefaultHorizontalPodAutoscalerConfig(),
			},
			ScaleTargetRef: &v1alpha1.ScaleTarget{
				Name:       is.Name,
				APIVersion: inferenceSetAPIVersion,
				Kind:       InferenceSet,
			},
			PollingInterval: ptr.To(int32(defaultPollingInterval)),
			MinReplicaCount: ptr.To(int32(minReplicas)),
			MaxReplicaCount: ptr.To(int32(maxReplicas)),
			Triggers:        getDefaultKedaKaitoScalerTriggers(is.Name, is.Namespace, c.ScalerNamespace, threshold, metricName),
		},
	}
}

func (c *Controller) updateScaledObject(ctx context.Context, is *kaitov1alpha1.InferenceSet, so *v1alpha1.ScaledObject, minReplicas, maxReplicas int, threshold, metricName string) error {
	updated := false

	if so.Spec.MinReplicaCount == nil || *so.Spec.MinReplicaCount != int32(minReplicas) {
		so.Spec.MinReplicaCount = ptr.To(int32(minReplicas))
		updated = true
	}
	if so.Spec.MaxReplicaCount == nil || *so.Spec.MaxReplicaCount != int32(maxReplicas) {
		so.Spec.MaxReplicaCount = ptr.To(int32(maxReplicas))
		updated = true
	}
	if len(so.Spec.Triggers) == 0 {
		// Restore triggers if they were removed (e.g., manual edit drift).
		so.Spec.Triggers = getDefaultKedaKaitoScalerTriggers(is.Name, is.Namespace, c.ScalerNamespace, threshold, metricName)
		updated = true
	} else {
		if so.Spec.Triggers[0].Metadata == nil {
			so.Spec.Triggers[0].Metadata = make(map[string]string)
		}
		if so.Spec.Triggers[0].Metadata["threshold"] != threshold {
			so.Spec.Triggers[0].Metadata["threshold"] = threshold
			updated = true
		}
		if so.Spec.Triggers[0].Metadata[scaler.MetricNameInMetadata] != metricName {
			so.Spec.Triggers[0].Metadata[scaler.MetricNameInMetadata] = metricName
			updated = true
		}
	}

	if !updated {
		return nil
	}
	return c.Update(ctx, so)
}

func generateInferenceSetPredicateFunc() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			inferenceSet, ok := e.Object.(*kaitov1alpha1.InferenceSet)
			if !ok {
				return false
			}
			return enableAutoProvisioning(inferenceSet)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldInferenceSet, ok := e.ObjectOld.(*kaitov1alpha1.InferenceSet)
			if !ok {
				return false
			}

			newInferenceSet, ok := e.ObjectNew.(*kaitov1alpha1.InferenceSet)
			if !ok {
				return false
			}

			// Reconcile when any watched annotation changes.
			for _, key := range watchedAnnotations {
				if oldInferenceSet.Annotations[key] != newInferenceSet.Annotations[key] {
					return enableAutoProvisioning(newInferenceSet)
				}
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

func resolveMinReplicas(annotations map[string]string) int {
	if minReplicasStr, ok := annotations[AnnotationKeyMinReplicas]; ok {
		if v, err := strconv.Atoi(minReplicasStr); err == nil && v > 1 {
			if v > math.MaxInt32 {
				return math.MaxInt32
			}
			return v
		}
	}
	return 1
}

// resolveMetricName returns the metric name configured via the metricName
// annotation on the InferenceSet, falling back to defaultMetricName when
// the annotation is absent or empty.
func resolveMetricName(annotations map[string]string) string {
	if name, ok := annotations[AnnotationKeyMetricName]; ok && name != "" {
		return name
	}
	return defaultMetricName
}

func enableAutoProvisioning(inferenceSet *kaitov1alpha1.InferenceSet) bool {
	if inferenceSet.Annotations[AnnotationKeyAutoProvision] != "true" {
		return false
	}

	// if max-replicas annotation exists, max replicas should be more than 1
	// if not exists, NodeCountLimit should be more than 1, we will use it to calculate max replicas
	if maxReplicasStr, ok := inferenceSet.Annotations[AnnotationKeyMaxReplicas]; ok {
		if maxReplicas, err := strconv.Atoi(maxReplicasStr); err != nil || maxReplicas <= 1 {
			return false
		}
	} else if inferenceSet.Spec.NodeCountLimit == 0 {
		return false
	}

	// threshold should be a valid integer
	if threshold, err := strconv.Atoi(inferenceSet.Annotations[AnnotationKeyThreshold]); err != nil || threshold < 0 {
		return false
	}

	return true
}

// +kubebuilder:rbac:groups="keda.sh",resources=scaledobjects,verbs=create;list;watch;get;update;delete
// +kubebuilder:rbac:groups="kaito.sh",resources=inferencesets,verbs=list;watch;get
// +kubebuilder:rbac:groups="kaito.sh",resources=workspaces,verbs=list;watch

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("scaledobject.provision").
		For(&kaitov1alpha1.InferenceSet{}, builder.WithPredicates(generateInferenceSetPredicateFunc())).
		Watches(&v1alpha1.ScaledObject{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			if scaledObj, ok := obj.(*v1alpha1.ScaledObject); ok {
				if scaledObj.Spec.ScaleTargetRef.Kind == InferenceSet {
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

func getDefaultHorizontalPodAutoscalerConfig() *v1alpha1.HorizontalPodAutoscalerConfig {
	return &v1alpha1.HorizontalPodAutoscalerConfig{
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(60)),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: 300,
					},
				},
				Tolerance: func() *resource.Quantity {
					q := resource.MustParse("0.1")
					return &q
				}(),
			},
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(300)),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: 600,
					},
				},
				Tolerance: func() *resource.Quantity {
					q := resource.MustParse("0.5")
					return &q
				}(),
			},
		},
	}
}

func getDefaultKedaKaitoScalerTriggers(inferenceSetName, inferenceSetNamespace, scalerNamespace, threshold, metricName string) []v1alpha1.ScaleTriggers {
	// metricProtocol/metricPort/metricPath/scrapeTimeout are intentionally
	// omitted: the scaler-side parseScalerMetadata fills them with defaults
	// matching Kaito's vLLM exposure (http on Service port 80, /metrics, 3s),
	// so we only keep keys that the scaler cannot infer on its own.
	return []v1alpha1.ScaleTriggers{
		{
			Type: "external",
			Name: scaler.ScalerName,
			Metadata: map[string]string{
				"threshold":                            threshold,
				scaler.InferenceSetNameInMetadata:      inferenceSetName,
				scaler.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
				scaler.ScalerAddressInMetadata:         fmt.Sprintf("keda-kaito-scaler-svc.%s.svc.cluster.local:%d", scalerNamespace, 10450),
				scaler.MetricNameInMetadata:            metricName,
			},
			AuthenticationRef: &v1alpha1.AuthenticationRef{
				Name: "keda-kaito-scaler-creds",
				Kind: "ClusterTriggerAuthentication",
			},
			MetricType: autoscalingv2.AverageValueMetricType,
		},
	}
}
