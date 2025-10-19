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
	"sort"
	"strconv"
	"time"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	"github.com/kaito-project/kaito/pkg/utils/inferenceset"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	AnnotationKeyAutoProvision = "scaledobject.kaito.sh/auto-provision"
	AnnotationKeyMaxReplicas   = "scaledobject.kaito.sh/max-replicas"
	AnnotationKeyThreshold     = "scaledobject.kaito.sh/threshold"

	AnnotationKeyManagedBy = "scaledobject.kaito.sh/managed-by"
)

type Controller struct {
	client.Client
}

func NewAutoProvisionController(c client.Client) *Controller {
	return &Controller{
		Client: c,
	}
}

func (c *Controller) Reconcile(ctx context.Context, is *kaitov1alpha1.InferenceSet) (reconcile.Result, error) {
	if !enableAutoProvisioning(is) {
		return reconcile.Result{}, nil
	}

	var maxReplicas int
	if maxReplicasStr, ok := is.Annotations[AnnotationKeyMaxReplicas]; ok {
		maxReplicas, _ = strconv.Atoi(maxReplicasStr)
	} else {
		// get the related workspace instances
		workspaceList, err := inferenceset.ListWorkspaces(ctx, is, c.Client)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to list workspaces for inference set %s/%s: %v", is.Namespace, is.Name, err)
		}

		var targetNodeCount int32
		for i := range workspaceList.Items {
			if workspaceList.Items[i].Status.TargetNodeCount != 0 {
				targetNodeCount = workspaceList.Items[i].Status.TargetNodeCount
				break
			}
		}

		if targetNodeCount == 0 {
			return reconcile.Result{}, fmt.Errorf("failed to get target node count from workspaces for inference set %s/%s", is.Namespace, is.Name)
		}

		maxReplicas = is.Spec.NodeCountLimit / int(targetNodeCount)
		if maxReplicas < 1 {
			maxReplicas = 1
		}
	}

	threshold := is.Annotations[AnnotationKeyThreshold]

	// list all scaled objects in the same namespace
	var scaledObjectList v1alpha1.ScaledObjectList
	if err := c.List(ctx, &scaledObjectList, client.InNamespace(is.Namespace)); err != nil {
		return reconcile.Result{}, err
	}

	// filter the scaled objects related to the workspace and auto provisioning
	var filteredScaledObjects []v1alpha1.ScaledObject
	for _, scaledObject := range scaledObjectList.Items {
		if scaledObject.Spec.ScaleTargetRef.Name == is.Name && scaledObject.Spec.ScaleTargetRef.Kind == "InferenceSet" {
			if scaledObject.Annotations[AnnotationKeyManagedBy] == "keda-kaito-scaler" {
				filteredScaledObjects = append(filteredScaledObjects, scaledObject)
			}
		}
	}

	// if no scaled object found, create one
	if len(filteredScaledObjects) == 0 {
		newScaledObject := &v1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      is.Name,
				Namespace: is.Namespace,
				Annotations: map[string]string{
					AnnotationKeyManagedBy: "keda-kaito-scaler",
				},
			},
			Spec: v1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &v1alpha1.ScaleTarget{
					Name:       is.Name,
					APIVersion: "kaito.sh/v1alpha1",
					Kind:       "InferenceSet",
				},
				MinReplicaCount: ptr.To(int32(1)),
				MaxReplicaCount: ptr.To(int32(maxReplicas)),
				Triggers: []v1alpha1.ScaleTriggers{
					{
						Type: "external",
						Name: "keda-kaito-scaler",
						Metadata: map[string]string{
							"scalerName": "keda-kaito-scaler",
							"threshold":  threshold,
						},
					},
				},
			},
		}
		if err := c.Create(ctx, newScaledObject); err != nil {
			return reconcile.Result{}, err
		}
	} else if len(filteredScaledObjects) == 1 {
		// update the existing scaled object if annotation values changed
		existingScaledObject := filteredScaledObjects[0]
		updated := false

		if *existingScaledObject.Spec.MaxReplicaCount != int32(maxReplicas) {
			existingScaledObject.Spec.MaxReplicaCount = ptr.To(int32(maxReplicas))
			updated = true
		}
		if existingScaledObject.Spec.Triggers[0].Metadata["threshold"] != threshold {
			existingScaledObject.Spec.Triggers[0].Metadata["threshold"] = threshold
			updated = true
		}

		if updated {
			if err := c.Update(ctx, &existingScaledObject); err != nil {
				return reconcile.Result{}, err
			}
		}
	} else {
		// sort by creation timestamp, and delete new ones.
		sort.SliceStable(filteredScaledObjects, func(i, j int) bool {
			return filteredScaledObjects[i].CreationTimestamp.Before(&filteredScaledObjects[j].CreationTimestamp)
		})

		for i := 1; i < len(filteredScaledObjects); i++ {
			if err := c.Delete(ctx, &filteredScaledObjects[i]); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	return reconcile.Result{}, nil
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

			// check auto-provision, max-replicas, threshold annotation changes
			if oldInferenceSet.Annotations[AnnotationKeyAutoProvision] != newInferenceSet.Annotations[AnnotationKeyAutoProvision] ||
				oldInferenceSet.Annotations[AnnotationKeyMaxReplicas] != newInferenceSet.Annotations[AnnotationKeyMaxReplicas] ||
				oldInferenceSet.Annotations[AnnotationKeyThreshold] != newInferenceSet.Annotations[AnnotationKeyThreshold] {
				return enableAutoProvisioning(newInferenceSet)
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
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
				if scaledObj.Spec.ScaleTargetRef.Kind == "InferenceSet" {
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
