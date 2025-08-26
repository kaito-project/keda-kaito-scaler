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

package scaledobject

import (
	"context"
	"sort"
	"strconv"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
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

func (c *Controller) Reconcile(ctx context.Context, ws *kaitov1beta1.Workspace) (reconcile.Result, error) {
	if !enableAutoProvisioning(ws) {
		return reconcile.Result{}, nil
	}

	maxReplicas, _ := strconv.Atoi(ws.Annotations[AnnotationKeyMaxReplicas])
	threshold := ws.Annotations[AnnotationKeyThreshold]

	// list all scaled objects in the same namespace
	var scaledObjectList v1alpha1.ScaledObjectList
	if err := c.List(ctx, &scaledObjectList, client.InNamespace(ws.Namespace)); err != nil {
		return reconcile.Result{}, err
	}

	// filter the scaled objects related to the workspace and auto provisioning
	var filteredScaledObjects []v1alpha1.ScaledObject
	for _, scaledObject := range scaledObjectList.Items {
		if scaledObject.Spec.ScaleTargetRef.Name == ws.Name && scaledObject.Spec.ScaleTargetRef.Kind == "Workspace" {
			if scaledObject.Annotations[AnnotationKeyManagedBy] == "keda-kaito-scaler" {
				filteredScaledObjects = append(filteredScaledObjects, scaledObject)
			}
		}
	}

	// if no scaled object found, create one
	if len(filteredScaledObjects) == 0 {
		newScaledObject := &v1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ws.Name,
				Namespace: ws.Namespace,
				Annotations: map[string]string{
					AnnotationKeyManagedBy: "keda-kaito-scaler",
				},
			},
			Spec: v1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &v1alpha1.ScaleTarget{
					Name:       ws.Name,
					APIVersion: "kaito.sh/v1beta1",
					Kind:       "Workspace",
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

func generateWorkspacePredicateFunc() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			workspace, ok := e.Object.(*kaitov1beta1.Workspace)
			if !ok {
				return false
			}
			return enableAutoProvisioning(workspace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldWorkspace, ok := e.ObjectOld.(*kaitov1beta1.Workspace)
			if !ok {
				return false
			}

			newWorkspace, ok := e.ObjectNew.(*kaitov1beta1.Workspace)
			if !ok {
				return false
			}

			// check auto-provision, max-replicas, threshold annotation changes
			if oldWorkspace.Annotations[AnnotationKeyAutoProvision] != newWorkspace.Annotations[AnnotationKeyAutoProvision] ||
				oldWorkspace.Annotations[AnnotationKeyMaxReplicas] != newWorkspace.Annotations[AnnotationKeyMaxReplicas] ||
				oldWorkspace.Annotations[AnnotationKeyThreshold] != newWorkspace.Annotations[AnnotationKeyThreshold] {
				return enableAutoProvisioning(newWorkspace)
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

func enableAutoProvisioning(workspace *kaitov1beta1.Workspace) bool {
	if workspace.Annotations[AnnotationKeyAutoProvision] != "true" {
		return false
	}

	// max replicas should be more than 1
	if maxReplicas, err := strconv.Atoi(workspace.Annotations[AnnotationKeyMaxReplicas]); err != nil || maxReplicas <= 1 {
		return false
	}

	// threshold should be a valid integer
	if threshold, err := strconv.Atoi(workspace.Annotations[AnnotationKeyThreshold]); err != nil || threshold < 0 {
		return false
	}

	return true
}

// +kubebuilder:rbac:groups="keda.sh",resources=scaledobjects,verbs=create;list;watch;get;update;delete
// +kubebuilder:rbac:groups="kaito.sh",resources=workspace,verbs=list;watch;get

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("scaledobject.provision").
		For(&kaitov1beta1.Workspace{}, builder.WithPredicates(generateWorkspacePredicateFunc())).
		Watches(&v1alpha1.ScaledObject{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			if scaledObj, ok := obj.(*v1alpha1.ScaledObject); ok {
				if scaledObj.Spec.ScaleTargetRef.Kind != "Workspace" {
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
