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
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaledobject"
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
			annotations: map[string]string{AnnotationKeyMinReplicas: "0"},
			expected:    1,
		},
		{
			name:        "annotation value equal to 1 returns 1",
			annotations: map[string]string{AnnotationKeyMinReplicas: "1"},
			expected:    1,
		},
		{
			name:        "negative annotation value is clamped to 1",
			annotations: map[string]string{AnnotationKeyMinReplicas: "-5"},
			expected:    1,
		},
		{
			name:        "invalid annotation value falls back to 1",
			annotations: map[string]string{AnnotationKeyMinReplicas: "abc"},
			expected:    1,
		},
		{
			name:        "valid value greater than 1 is returned",
			annotations: map[string]string{AnnotationKeyMinReplicas: "3"},
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
			is:            newInferenceSet(0, map[string]string{AnnotationKeyMaxReplicas: "5"}),
			expectedValue: 5,
		},
		{
			name:          "NodeCountLimit=0 with annotation <1 is clamped to 1",
			is:            newInferenceSet(0, map[string]string{AnnotationKeyMaxReplicas: "0"}),
			expectedValue: 1,
		},
		{
			name:          "NodeCountLimit=0 with invalid annotation is clamped to 1",
			is:            newInferenceSet(0, map[string]string{AnnotationKeyMaxReplicas: "abc"}),
			expectedValue: 1,
		},
		{
			name:          "NodeCountLimit=0 with annotation exceeding MaxInt32 is clamped to MaxInt32",
			is:            newInferenceSet(0, map[string]string{AnnotationKeyMaxReplicas: fmt.Sprintf("%d", int64(math.MaxInt32)+1)}),
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
		AnnotationKeyAutoProvision:              "true",
		AnnotationKeyMaxReplicas:                "5",
		"scaledobject.kaito.sh/metricName/0":    "inference_pool_per_pod_queue_size",
		"scaledobject.kaito.sh/upthreshold/0":   "10",
		"scaledobject.kaito.sh/downthreshold/0": "2",
		"scaledobject.kaito.sh/metricName/1":    "inference_objective_request_duration_seconds",
		"scaledobject.kaito.sh/upthreshold/1":   "1.5",
		"scaledobject.kaito.sh/downthreshold/1": "0.5",
	}
}

func TestEnableAutoProvisioning_Composite(t *testing.T) {
	t.Run("valid composite enabled", func(t *testing.T) {
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: compositeAnnotations()}}
		assert.True(t, enableAutoProvisioning(is))
	})

	t.Run("invalid composite disabled", func(t *testing.T) {
		ann := compositeAnnotations()
		ann["scaledobject.kaito.sh/metricName/0"] = "bogus"
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
		assert.False(t, enableAutoProvisioning(is))
	})

	t.Run("single metric mode still uses scaledobject schema", func(t *testing.T) {
		is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			AnnotationKeyAutoProvision:           "true",
			AnnotationKeyMaxReplicas:             "5",
			scaledobject.AnnotationKeyThreshold:  "10",
			scaledobject.AnnotationKeyMetricName: "vllm:num_requests_waiting",
		}}}
		assert.True(t, enableAutoProvisioning(is))
	})
}

func TestAnnotationsWithPrefixChanged(t *testing.T) {
	old := map[string]string{"scaledobject.kaito.sh/metricName/0": "queue", "other": "x"}

	// Changing an indexed composite key is detected.
	next := map[string]string{"scaledobject.kaito.sh/metricName/0": "latency-p95", "other": "x"}
	assert.True(t, annotationsWithPrefixChanged(old, next, annotationPrefix))

	// Adding a new prefixed key is detected.
	added := map[string]string{"scaledobject.kaito.sh/metricName/0": "queue", "scaledobject.kaito.sh/upthreshold/0": "10", "other": "x"}
	assert.True(t, annotationsWithPrefixChanged(old, added, annotationPrefix))

	// Changing a non-prefixed key is ignored.
	unrelated := map[string]string{"scaledobject.kaito.sh/metricName/0": "queue", "other": "y"}
	assert.False(t, annotationsWithPrefixChanged(old, unrelated, annotationPrefix))
}
