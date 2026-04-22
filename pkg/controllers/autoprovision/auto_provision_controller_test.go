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

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
)

func TestGetDefaultKedaKaitoScalerTriggers(t *testing.T) {
	tests := []struct {
		name                  string
		inferenceSetName      string
		inferenceSetNamespace string
		scalerNamespace       string
		threshold             string
		expectedTriggerCount  int
		expectedType          string
		expectedName          string
		expectedMetricType    autoscalingv2.MetricTargetType
		expectedAuthRefName   string
		expectedAuthRefKind   string
	}{
		{
			name:                  "basic trigger creation",
			inferenceSetName:      "test-inference-set",
			inferenceSetNamespace: "test-namespace",
			scalerNamespace:       "kaito-workspace",
			threshold:             "10",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
		{
			name:                  "different threshold value",
			inferenceSetName:      "another-inference-set",
			inferenceSetNamespace: "another-namespace",
			scalerNamespace:       "kaito-workspace",
			threshold:             "5",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
		{
			name:                  "empty threshold",
			inferenceSetName:      "test-inference-set",
			inferenceSetNamespace: "test-namespace",
			scalerNamespace:       "kaito-workspace",
			threshold:             "",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			triggers := getDefaultKedaKaitoScalerTriggers(tt.inferenceSetName, tt.inferenceSetNamespace, tt.scalerNamespace, tt.threshold)

			// Check trigger count
			assert.Equal(t, tt.expectedTriggerCount, len(triggers))

			if len(triggers) > 0 {
				trigger := triggers[0]

				// Check basic properties
				assert.Equal(t, tt.expectedType, trigger.Type)
				assert.Equal(t, tt.expectedName, trigger.Name)
				assert.Equal(t, tt.expectedMetricType, trigger.MetricType)

				// Check authentication reference
				assert.NotNil(t, trigger.AuthenticationRef)
				assert.Equal(t, tt.expectedAuthRefName, trigger.AuthenticationRef.Name)
				assert.Equal(t, tt.expectedAuthRefKind, trigger.AuthenticationRef.Kind)

				// Check metadata
				assert.NotNil(t, trigger.Metadata)
				assert.Equal(t, "keda-kaito-scaler", trigger.Metadata["scalerName"])
				assert.Equal(t, tt.threshold, trigger.Metadata["threshold"])
				assert.Equal(t, tt.inferenceSetName, trigger.Metadata[scaler.InferenceSetNameInMetadata])
				assert.Equal(t, tt.inferenceSetNamespace, trigger.Metadata[scaler.InferenceSetNamespaceInMetadata])
				assert.Equal(t, fmt.Sprintf("keda-kaito-scaler-svc.%s.svc.cluster.local:%d", tt.scalerNamespace, 10450), trigger.Metadata[scaler.ScalerAddressInMetadata])
				assert.Equal(t, "vllm:num_requests_waiting", trigger.Metadata[scaler.MetricNameInMetadata])
				assert.Equal(t, "http", trigger.Metadata[scaler.MetricProtocolInMetadata])
				assert.Equal(t, "80", trigger.Metadata[scaler.MetricPortInMetadata])
				assert.Equal(t, "/metrics", trigger.Metadata[scaler.MetricPathInMetadata])
				assert.Equal(t, "5s", trigger.Metadata[scaler.ScrapeTimeoutInMetadata])
			}
		})
	}
}

func TestGetDefaultKedaKaitoScalerTriggers_MetadataKeys(t *testing.T) {
	inferenceSetName := "test-inference-set"
	inferenceSetNamespace := "test-namespace"
	scalerNamespace := "kaito-workspace"
	threshold := "10"

	triggers := getDefaultKedaKaitoScalerTriggers(inferenceSetName, inferenceSetNamespace, scalerNamespace, threshold)

	assert.Equal(t, 1, len(triggers))
	trigger := triggers[0]

	// Verify all expected metadata keys are present
	expectedKeys := []string{
		"scalerName",
		"threshold",
		scaler.InferenceSetNameInMetadata,
		scaler.InferenceSetNamespaceInMetadata,
		scaler.ScalerAddressInMetadata,
		scaler.MetricNameInMetadata,
		scaler.MetricProtocolInMetadata,
		scaler.MetricPortInMetadata,
		scaler.MetricPathInMetadata,
		scaler.ScrapeTimeoutInMetadata,
	}

	for _, key := range expectedKeys {
		assert.Contains(t, trigger.Metadata, key, "Expected metadata key %s to be present", key)
	}

	// Verify metadata count matches expected keys
	assert.Equal(t, len(expectedKeys), len(trigger.Metadata))
}

func TestGetDefaultHorizontalPodAutoscalerConfig(t *testing.T) {
	config := getDefaultHorizontalPodAutoscalerConfig()

	// Verify config is not nil
	assert.NotNil(t, config)
	assert.NotNil(t, config.Behavior)

	// Test ScaleUp configuration
	assert.NotNil(t, config.Behavior.ScaleUp)
	scaleUp := config.Behavior.ScaleUp

	// Check ScaleUp stabilization window
	assert.NotNil(t, scaleUp.StabilizationWindowSeconds)
	assert.Equal(t, int32(60), *scaleUp.StabilizationWindowSeconds)

	// Check ScaleUp select policy
	assert.NotNil(t, scaleUp.SelectPolicy)
	assert.Equal(t, autoscalingv2.MaxChangePolicySelect, *scaleUp.SelectPolicy)

	// Check ScaleUp policies
	assert.Len(t, scaleUp.Policies, 1)
	scaleUpPolicy := scaleUp.Policies[0]
	assert.Equal(t, autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy), scaleUpPolicy.Type)
	assert.Equal(t, int32(1), scaleUpPolicy.Value)
	assert.Equal(t, int32(300), scaleUpPolicy.PeriodSeconds)

	// Check ScaleUp tolerance
	assert.NotNil(t, scaleUp.Tolerance)
	expectedScaleUpTolerance := resource.MustParse("0.1")
	assert.True(t, scaleUp.Tolerance.Equal(expectedScaleUpTolerance))

	// Test ScaleDown configuration
	assert.NotNil(t, config.Behavior.ScaleDown)
	scaleDown := config.Behavior.ScaleDown

	// Check ScaleDown stabilization window
	assert.NotNil(t, scaleDown.StabilizationWindowSeconds)
	assert.Equal(t, int32(300), *scaleDown.StabilizationWindowSeconds)

	// Check ScaleDown select policy
	assert.NotNil(t, scaleDown.SelectPolicy)
	assert.Equal(t, autoscalingv2.MaxChangePolicySelect, *scaleDown.SelectPolicy)

	// Check ScaleDown policies
	assert.Len(t, scaleDown.Policies, 1)
	scaleDownPolicy := scaleDown.Policies[0]
	assert.Equal(t, autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy), scaleDownPolicy.Type)
	assert.Equal(t, int32(1), scaleDownPolicy.Value)
	assert.Equal(t, int32(600), scaleDownPolicy.PeriodSeconds)

	// Check ScaleDown tolerance
	assert.NotNil(t, scaleDown.Tolerance)
	expectedScaleDownTolerance := resource.MustParse("0.5")
	assert.True(t, scaleDown.Tolerance.Equal(expectedScaleDownTolerance))
}

func TestGetDefaultHorizontalPodAutoscalerConfig_Consistency(t *testing.T) {
	// Test that multiple calls return equivalent configurations
	config1 := getDefaultHorizontalPodAutoscalerConfig()
	config2 := getDefaultHorizontalPodAutoscalerConfig()

	// Verify both configurations are not nil
	assert.NotNil(t, config1)
	assert.NotNil(t, config2)

	// Verify ScaleUp configurations are equivalent
	assert.Equal(t, *config1.Behavior.ScaleUp.StabilizationWindowSeconds, *config2.Behavior.ScaleUp.StabilizationWindowSeconds)
	assert.Equal(t, *config1.Behavior.ScaleUp.SelectPolicy, *config2.Behavior.ScaleUp.SelectPolicy)
	assert.True(t, config1.Behavior.ScaleUp.Tolerance.Equal(*config2.Behavior.ScaleUp.Tolerance))

	// Verify ScaleDown configurations are equivalent
	assert.Equal(t, *config1.Behavior.ScaleDown.StabilizationWindowSeconds, *config2.Behavior.ScaleDown.StabilizationWindowSeconds)
	assert.Equal(t, *config1.Behavior.ScaleDown.SelectPolicy, *config2.Behavior.ScaleDown.SelectPolicy)
	assert.True(t, config1.Behavior.ScaleDown.Tolerance.Equal(*config2.Behavior.ScaleDown.Tolerance))
}

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
	assert.NoError(t, kaitov1alpha1.AddToScheme(scheme))
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestResolveMaxReplicas(t *testing.T) {
	ctx := context.Background()
	const inferenceSetName = "test-is"
	const inferenceSetNamespace = "default"

	newInferenceSet := func(nodeCountLimit int, annotations map[string]string) *kaitov1alpha1.InferenceSet {
		return &kaitov1alpha1.InferenceSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:        inferenceSetName,
				Namespace:   inferenceSetNamespace,
				Annotations: annotations,
			},
			Spec: kaitov1alpha1.InferenceSetSpec{
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
		is              *kaitov1alpha1.InferenceSet
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
