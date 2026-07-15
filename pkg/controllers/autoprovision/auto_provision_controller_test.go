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
	"testing"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
)

func TestResolveMinReplicas(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    int
	}{
		{
			name:        "annotation not set returns 1",
			annotations: nil,
			expected:    1,
		},
		{
			name:        "empty annotations returns 1",
			annotations: map[string]string{},
			expected:    1,
		},
		{
			name:        "annotation value less than 1 is clamped to 1",
			annotations: map[string]string{constants.AnnotationKeyMinReplicas: "0"},
			expected:    1,
		},
		{
			name:        "annotation value equal to 1 returns 1",
			annotations: map[string]string{constants.AnnotationKeyMinReplicas: "1"},
			expected:    1,
		},
		{
			name:        "negative annotation value is clamped to 1",
			annotations: map[string]string{constants.AnnotationKeyMinReplicas: "-5"},
			expected:    1,
		},
		{
			name:        "invalid annotation value falls back to 1",
			annotations: map[string]string{constants.AnnotationKeyMinReplicas: "abc"},
			expected:    1,
		},
		{
			name:        "valid value greater than 1 is returned",
			annotations: map[string]string{constants.AnnotationKeyMinReplicas: "3"},
			expected:    3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, resolveMinReplicas(tt.annotations))
		})
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestResolveMaxReplicas(t *testing.T) {
	ctx := context.Background()
	const inferenceSetName = "test-is"
	const inferenceSetNamespace = "default"

	newInferenceSet := func(nodeCountLimit int, annotations map[string]string) *kaitov1beta1.InferenceSet {
		return &kaitov1beta1.InferenceSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:        inferenceSetName,
				Namespace:   inferenceSetNamespace,
				Annotations: annotations,
			},
			Spec: kaitov1beta1.InferenceSetSpec{
				NodeCountLimit: nodeCountLimit,
			},
		}
	}

	newWorkspace := func(targetNodeCount int32) *kaitov1beta1.Workspace {
		return &kaitov1beta1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      inferenceSetName + "-0",
				Namespace: inferenceSetNamespace,
				Labels: map[string]string{
					"inferenceset.kaito.sh/created-by": inferenceSetName,
				},
			},
			Status: kaitov1beta1.WorkspaceStatus{
				TargetNodeCount: targetNodeCount,
			},
		}
	}

	tests := []struct {
		name            string
		is              *kaitov1beta1.InferenceSet
		workspaces      []client.Object
		expectedValue   int
		expectedRequeue bool
		expectError     bool
	}{
		{
			name:          "NodeCountLimit=0 without annotation returns default 1",
			is:            newInferenceSet(0, nil),
			expectedValue: 1,
		},
		{
			name:          "NodeCountLimit=0 with valid annotation returns annotation value",
			is:            newInferenceSet(0, map[string]string{constants.AnnotationKeyMaxReplicas: "5"}),
			expectedValue: 5,
		},
		{
			name:          "NodeCountLimit=0 with annotation <1 is clamped to 1",
			is:            newInferenceSet(0, map[string]string{constants.AnnotationKeyMaxReplicas: "0"}),
			expectedValue: 1,
		},
		{
			name:          "NodeCountLimit=0 with invalid annotation is clamped to 1",
			is:            newInferenceSet(0, map[string]string{constants.AnnotationKeyMaxReplicas: "abc"}),
			expectedValue: 1,
		},
		{
			name:          "NodeCountLimit=0 with annotation exceeding MaxInt32 is clamped to MaxInt32",
			is:            newInferenceSet(0, map[string]string{constants.AnnotationKeyMaxReplicas: fmt.Sprintf("%d", int64(math.MaxInt32)+1)}),
			expectedValue: math.MaxInt32,
		},
		{
			name:            "NodeCountLimit>0 with no workspaces requests requeue",
			is:              newInferenceSet(10, nil),
			workspaces:      nil,
			expectedRequeue: true,
		},
		{
			name:            "NodeCountLimit>0 with workspace targetNodeCount=0 requests requeue",
			is:              newInferenceSet(10, nil),
			workspaces:      []client.Object{newWorkspace(0)},
			expectedRequeue: true,
		},
		{
			name:          "NodeCountLimit>0 with workspace targetNodeCount=2 returns 5",
			is:            newInferenceSet(10, nil),
			workspaces:    []client.Object{newWorkspace(2)},
			expectedValue: 5,
		},
		{
			name:          "NodeCountLimit>0 with division result <1 is clamped to 1",
			is:            newInferenceSet(1, nil),
			workspaces:    []client.Object{newWorkspace(4)},
			expectedValue: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeClient(t, tt.workspaces...)
			ctrl := &Controller{Client: c}
			value, requeue, err := ctrl.resolveMaxReplicas(ctx, tt.is)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedRequeue, requeue)
			if !tt.expectedRequeue {
				assert.Equal(t, tt.expectedValue, value)
			}
		})
	}
}

// compositeAnnotations returns a valid composite autoscaling annotation set,
// including the gating annotations the controller requires to enable
// auto-provisioning.
func compositeAnnotations() map[string]string {
	return map[string]string{
		constants.AnnotationKeyAutoProvision: "true",
		constants.AnnotationKeyMaxReplicas:   "5",
		constants.AnnotationKeyMetrics: `
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 10
  downthreshold: 2
- name: vllm:request_queue_time_seconds
  type: histogram
  upthreshold: 1.5
  downthreshold: 0.5
`,
	}
}

func TestAutoscalingConfigValid_Composite(t *testing.T) {
	t.Run("valid composite enabled", func(t *testing.T) {
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: compositeAnnotations()}}
		assert.True(t, autoscalingConfigValid(is))
	})

	t.Run("invalid composite disabled", func(t *testing.T) {
		ann := compositeAnnotations()
		ann[constants.AnnotationKeyMetrics] = `
- name: vllm:num_requests_waiting
  type: bogus
  upthreshold: 10
  downthreshold: 2
`
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
		assert.False(t, autoscalingConfigValid(is))
	})

	t.Run("single metric config accepted", func(t *testing.T) {
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			constants.AnnotationKeyAutoProvision: "true",
			constants.AnnotationKeyMaxReplicas:   "5",
			constants.AnnotationKeyMetrics: `
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 5
  downthreshold: 1
`,
		}}}
		assert.True(t, autoscalingConfigValid(is))
	})

	t.Run("no metrics rejected", func(t *testing.T) {
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			constants.AnnotationKeyAutoProvision: "true",
			constants.AnnotationKeyMaxReplicas:   "5",
		}}}
		assert.False(t, autoscalingConfigValid(is))
	})
}

func TestAnnotationsWithPrefixChanged(t *testing.T) {
	old := map[string]string{"scaledobject.kaito.sh/metrics": "queue", "other": "x"}

	// Changing a composite key is detected.
	next := map[string]string{"scaledobject.kaito.sh/metrics": "latency-p95", "other": "x"}
	assert.True(t, annotationsWithPrefixChanged(old, next, annotationPrefix))

	// Adding a new prefixed key is detected.
	added := map[string]string{"scaledobject.kaito.sh/metrics": "queue", "scaledobject.kaito.sh/max-replicas": "10", "other": "x"}
	assert.True(t, annotationsWithPrefixChanged(old, added, annotationPrefix))

	// Changing a non-prefixed key is ignored.
	unrelated := map[string]string{"scaledobject.kaito.sh/metrics": "queue", "other": "y"}
	assert.False(t, annotationsWithPrefixChanged(old, unrelated, annotationPrefix))
}

func newReconcileFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	assert.NoError(t, v1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newManagedScaledObject(name string, is *kaitov1beta1.InferenceSet) *v1alpha1.ScaledObject {
	return &v1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: is.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "kaito.sh/v1beta1",
					Kind:       constants.InferenceSet,
					Name:       is.Name,
					UID:        is.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &v1alpha1.ScaleTarget{Name: is.Name, Kind: constants.InferenceSet},
		},
	}
}

// TestReconcile_CleanupWhenAutoProvisionDisabled verifies that toggling the
// auto-provision annotation to a non-"true" value deletes the ScaledObject the
// controller previously provisioned, while leaving user-managed ScaledObjects
// untouched.
func TestReconcile_CleanupWhenAutoProvisionDisabled(t *testing.T) {
	const (
		isName = "test-is"
		ns     = "default"
	)

	newDisabledIS := func() *kaitov1beta1.InferenceSet {
		return &kaitov1beta1.InferenceSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:        isName,
				Namespace:   ns,
				UID:         types.UID("is-uid"),
				Annotations: map[string]string{constants.AnnotationKeyAutoProvision: "false"},
			},
		}
	}

	t.Run("deletes managed ScaledObject when auto-provision is false", func(t *testing.T) {
		is := newDisabledIS()
		so := newManagedScaledObject("test-is-so", is)
		c := newReconcileFakeClient(t, is, so)
		ctrl := &Controller{Client: c}

		_, err := ctrl.Reconcile(context.Background(), is)
		assert.NoError(t, err)

		var got v1alpha1.ScaledObject
		err = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "test-is-so"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "managed ScaledObject should be deleted")
	})

	t.Run("deletes managed ScaledObject when annotation is absent", func(t *testing.T) {
		is := newDisabledIS()
		is.Annotations = nil
		so := newManagedScaledObject("test-is-so", is)
		c := newReconcileFakeClient(t, is, so)
		ctrl := &Controller{Client: c}

		_, err := ctrl.Reconcile(context.Background(), is)
		assert.NoError(t, err)

		var got v1alpha1.ScaledObject
		err = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "test-is-so"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "managed ScaledObject should be deleted")
	})

	t.Run("preserves ScaledObject not owned by the InferenceSet", func(t *testing.T) {
		is := newDisabledIS()
		// Hand-created ScaledObject with no controller owner reference: not managed.
		so := &v1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Name: "user-so", Namespace: ns},
			Spec: v1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &v1alpha1.ScaleTarget{Name: isName, Kind: constants.InferenceSet},
			},
		}
		c := newReconcileFakeClient(t, is, so)
		ctrl := &Controller{Client: c}

		_, err := ctrl.Reconcile(context.Background(), is)
		assert.NoError(t, err)

		var got v1alpha1.ScaledObject
		err = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "user-so"}, &got)
		assert.NoError(t, err, "user-managed ScaledObject should be preserved")
	})
}
