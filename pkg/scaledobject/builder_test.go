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
	"fmt"
	"testing"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
)

func TestGetDefaultKedaKaitoScalerTriggers(t *testing.T) {
	const (
		testScalerServiceName = "keda-kaito-scaler-svc"
		testScalerGRPCPort    = 10450
	)
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
			expectedAuthRefName:   scaler.ClusterTriggerAuthName,
			expectedAuthRefKind:   scaler.ClusterTriggerAuthKind,
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
			expectedAuthRefName:   scaler.ClusterTriggerAuthName,
			expectedAuthRefKind:   scaler.ClusterTriggerAuthKind,
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
			expectedAuthRefName:   scaler.ClusterTriggerAuthName,
			expectedAuthRefKind:   scaler.ClusterTriggerAuthKind,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			triggers := getDefaultKedaKaitoScalerTriggers(tt.inferenceSetName, tt.inferenceSetNamespace, tt.scalerNamespace, testScalerServiceName, testScalerGRPCPort, tt.threshold, defaultMetricName)

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
				assert.Equal(t, tt.threshold, trigger.Metadata["threshold"])
				assert.Equal(t, tt.inferenceSetName, trigger.Metadata[scaler.InferenceSetNameInMetadata])
				assert.Equal(t, tt.inferenceSetNamespace, trigger.Metadata[scaler.InferenceSetNamespaceInMetadata])
				assert.Equal(t, fmt.Sprintf("%s.%s.svc.cluster.local:%d", testScalerServiceName, tt.scalerNamespace, testScalerGRPCPort), trigger.Metadata[scaler.ScalerAddressInMetadata])
				assert.Equal(t, "vllm:num_requests_waiting", trigger.Metadata[scaler.MetricNameInMetadata])
				// Optional scrape settings (protocol/port/path/timeout) are intentionally
				// omitted from the trigger metadata so the scaler can apply its defaults.
				assert.NotContains(t, trigger.Metadata, scaler.MetricProtocolInMetadata)
				assert.NotContains(t, trigger.Metadata, scaler.MetricPortInMetadata)
				assert.NotContains(t, trigger.Metadata, scaler.MetricPathInMetadata)
				assert.NotContains(t, trigger.Metadata, scaler.ScrapeTimeoutInMetadata)
			}
		})
	}
}

func TestGetDefaultKedaKaitoScalerTriggers_MetadataKeys(t *testing.T) {
	inferenceSetName := "test-inference-set"
	inferenceSetNamespace := "test-namespace"
	scalerNamespace := "kaito-workspace"
	threshold := "10"

	triggers := getDefaultKedaKaitoScalerTriggers(inferenceSetName, inferenceSetNamespace, scalerNamespace, "keda-kaito-scaler-svc", 10450, threshold, defaultMetricName)

	assert.Equal(t, 1, len(triggers))
	trigger := triggers[0]

	// Verify all expected metadata keys are present
	expectedKeys := []string{
		"threshold",
		scaler.InferenceSetNameInMetadata,
		scaler.InferenceSetNamespaceInMetadata,
		scaler.ScalerAddressInMetadata,
		scaler.MetricNameInMetadata,
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
	assert.Equal(t, int32(300), scaleDownPolicy.PeriodSeconds)

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

func TestBuildScaledObject(t *testing.T) {
	const (
		inferenceSetName      = "test-is"
		inferenceSetNamespace = "default"
		scalerNamespace       = "kaito-workspace"
		threshold             = "10"
		minReplicas           = 2
		maxReplicas           = 5
	)

	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inferenceSetName,
			Namespace: inferenceSetNamespace,
			UID:       "test-uid",
		},
	}

	b := Builder{ScalerNamespace: scalerNamespace}
	so := b.buildScaledObject(is, minReplicas, maxReplicas, threshold, defaultMetricName)

	// Object meta
	assert.Equal(t, inferenceSetName, so.Name)
	assert.Equal(t, inferenceSetNamespace, so.Namespace)
	assert.Equal(t, scaler.ScalerName, so.Annotations[AnnotationKeyManagedBy])
	assert.Len(t, so.OwnerReferences, 1)
	assert.Equal(t, InferenceSet, so.OwnerReferences[0].Kind)
	assert.Equal(t, inferenceSetName, so.OwnerReferences[0].Name)

	// PollingInterval defaults to 15s so KEDA polls the external scaler more
	// frequently than its built-in 30s default.
	assert.NotNil(t, so.Spec.PollingInterval)
	assert.Equal(t, int32(defaultPollingInterval), *so.Spec.PollingInterval)
	assert.Equal(t, int32(15), *so.Spec.PollingInterval)

	// Replica bounds
	assert.NotNil(t, so.Spec.MinReplicaCount)
	assert.Equal(t, int32(minReplicas), *so.Spec.MinReplicaCount)
	assert.NotNil(t, so.Spec.MaxReplicaCount)
	assert.Equal(t, int32(maxReplicas), *so.Spec.MaxReplicaCount)

	// Scale target
	assert.NotNil(t, so.Spec.ScaleTargetRef)
	assert.Equal(t, inferenceSetName, so.Spec.ScaleTargetRef.Name)
	assert.Equal(t, InferenceSet, so.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, inferenceSetAPIVersion, so.Spec.ScaleTargetRef.APIVersion)

	// Advanced HPA config and triggers wired in
	assert.NotNil(t, so.Spec.Advanced)
	assert.NotNil(t, so.Spec.Advanced.HorizontalPodAutoscalerConfig)
	assert.Len(t, so.Spec.Triggers, 1)
	assert.Equal(t, threshold, so.Spec.Triggers[0].Metadata["threshold"])
}

func compositeAnnotations() map[string]string {
	return map[string]string{
		compositeIndexedKey(AnnotationKeyMetricName, 0): "vllm:num_requests_waiting",
		"scaledobject.kaito.sh/upthreshold/0":           "10",
		"scaledobject.kaito.sh/downthreshold/0":         "2",
		compositeIndexedKey(AnnotationKeyMetricName, 1): "vllm:request_queue_time_seconds",
		"scaledobject.kaito.sh/upthreshold/1":           "1.5",
		"scaledobject.kaito.sh/downthreshold/1":         "0.5",
	}
}

func TestIsCompositeMode(t *testing.T) {
	assert.True(t, IsCompositeMode(compositeAnnotations()))
	assert.False(t, IsCompositeMode(map[string]string{AnnotationKeyMetricName: "queue"}))
	assert.False(t, IsCompositeMode(nil))
}

func TestParseCompositeConfig(t *testing.T) {
	t.Run("valid two-metric config", func(t *testing.T) {
		cfg, err := parseCompositeConfig(compositeAnnotations())
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 2)
		assert.Equal(t, "vllm:num_requests_waiting", cfg.metrics[0].key)
		assert.Equal(t, "service", cfg.metrics[0].entry.source)
		assert.Equal(t, "service-avg", cfg.metrics[0].entry.aggregation)
		assert.Equal(t, "10", cfg.metrics[0].upThreshold)
		assert.Equal(t, "2", cfg.metrics[0].downThreshold)
		assert.Equal(t, "vllm:request_queue_time_seconds", cfg.metrics[1].key)
		assert.Equal(t, "quantile", cfg.metrics[1].entry.aggregation)
		// Quantile defaults to p95 for quantile-aggregated metrics.
		assert.Equal(t, "0.95", cfg.metrics[1].quantile)
		// Defaults applied.
		assert.Equal(t, int32(defaultEvaluationWindow), cfg.evaluationWindow)
		assert.Equal(t, int32(defaultScaleUpCooldown), cfg.scaleUpCooldown)
		assert.Equal(t, int32(defaultScaleDownCooldown), cfg.scaleDownCooldown)
	})

	t.Run("custom windows and cooldowns", func(t *testing.T) {
		ann := compositeAnnotations()
		ann[AnnotationKeyEvaluationWindow] = "30"
		ann[AnnotationKeyScaleUpCooldown] = "120"
		ann[AnnotationKeyScaleDownCooldown] = "240"
		cfg, err := parseCompositeConfig(ann)
		assert.NoError(t, err)
		assert.Equal(t, int32(30), cfg.evaluationWindow)
		assert.Equal(t, int32(120), cfg.scaleUpCooldown)
		assert.Equal(t, int32(240), cfg.scaleDownCooldown)
	})

	t.Run("unknown metric rejected", func(t *testing.T) {
		ann := compositeAnnotations()
		ann[compositeIndexedKey(AnnotationKeyMetricName, 0)] = "bogus"
		_, err := parseCompositeConfig(ann)
		assert.Error(t, err)
	})

	t.Run("invalid up threshold rejected", func(t *testing.T) {
		ann := compositeAnnotations()
		ann["scaledobject.kaito.sh/upthreshold/0"] = "abc"
		_, err := parseCompositeConfig(ann)
		assert.Error(t, err)
	})

	t.Run("down greater than up rejected", func(t *testing.T) {
		ann := compositeAnnotations()
		ann["scaledobject.kaito.sh/downthreshold/0"] = "20"
		_, err := parseCompositeConfig(ann)
		assert.Error(t, err)
	})

	t.Run("non-AND combine policy rejected", func(t *testing.T) {
		ann := compositeAnnotations()
		ann[AnnotationKeyCombinePolicy] = "OR"
		_, err := parseCompositeConfig(ann)
		assert.Error(t, err)
	})

	t.Run("explicit AND accepted", func(t *testing.T) {
		ann := compositeAnnotations()
		ann[AnnotationKeyCombinePolicy] = "AND"
		_, err := parseCompositeConfig(ann)
		assert.NoError(t, err)
	})

	t.Run("no metrics rejected", func(t *testing.T) {
		_, err := parseCompositeConfig(map[string]string{"scaledobject.kaito.sh/auto-provision": "true"})
		assert.Error(t, err)
	})
}

func TestBuildCompositeFormula(t *testing.T) {
	cfg, err := parseCompositeConfig(compositeAnnotations())
	assert.NoError(t, err)
	got := buildCompositeFormula(cfg)
	want := "(readiness_gate == 0 && vllm_num_requests_waiting > 10 && vllm_request_queue_time_seconds > 1.5) ? 2.0 : ((vllm_num_requests_waiting < 2 && vllm_request_queue_time_seconds < 0.5) ? 0.5 : 1.0)"
	assert.Equal(t, want, got)
}

func TestBuildCompositeScaledObject(t *testing.T) {
	const (
		isName = "test-is"
		ns     = "default"
	)
	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns, UID: "uid"},
	}
	b := Builder{ScalerNamespace: "kaito-workspace", ScalerServiceName: "kaito-scaler", ScalerGRPCPort: 9443}

	cfg, err := parseCompositeConfig(compositeAnnotations())
	assert.NoError(t, err)

	so := b.buildCompositeScaledObject(is, 1, 5, cfg)

	// One trigger per metric plus the readiness gate.
	assert.Len(t, so.Spec.Triggers, 3)
	assert.Equal(t, "vllm_num_requests_waiting", so.Spec.Triggers[0].Name)
	assert.Equal(t, "vllm_request_queue_time_seconds", so.Spec.Triggers[1].Name)
	assert.Equal(t, "readiness_gate", so.Spec.Triggers[2].Name)

	// metric_0 -> vllm:num_requests_waiting via service + service-avg.
	m0 := so.Spec.Triggers[0].Metadata
	assert.Equal(t, "service", m0[scaler.MetricSourceInMetadata])
	assert.Equal(t, "service-avg", m0[scaler.AggregationInMetadata])
	assert.Equal(t, "vllm:num_requests_waiting", m0[scaler.MetricNameInMetadata])
	assert.Equal(t, "1", m0["threshold"])

	// metric_1 -> vllm:request_queue_time_seconds via service + quantile, carrying the default p95 quantile.
	m1 := so.Spec.Triggers[1].Metadata
	assert.Equal(t, "quantile", m1[scaler.AggregationInMetadata])
	assert.Equal(t, "0.95", m1[scaler.QuantileInMetadata])

	// gate trigger uses the gate aggregation.
	gate := so.Spec.Triggers[2].Metadata
	assert.Equal(t, scaler.AggregationGate, gate[scaler.AggregationInMetadata])

	// All composite triggers use Value metric type.
	for _, tr := range so.Spec.Triggers {
		assert.Equal(t, autoscalingv2.ValueMetricType, tr.MetricType)
		assert.Equal(t, scaler.ClusterTriggerAuthName, tr.AuthenticationRef.Name)
	}

	// scalingModifiers wired with target 1 and Value metric type.
	sm := so.Spec.Advanced.ScalingModifiers
	assert.Equal(t, "1", sm.Target)
	assert.Equal(t, autoscalingv2.ValueMetricType, sm.MetricType)
	assert.Equal(t, buildCompositeFormula(cfg), sm.Formula)

	// HPA behaviour tolerances tightened to 0.1 in both directions.
	behavior := so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior
	assert.InDelta(t, 0.1, behavior.ScaleUp.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 0.1, behavior.ScaleDown.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.Equal(t, int32(defaultEvaluationWindow), *behavior.ScaleUp.StabilizationWindowSeconds)
	assert.Equal(t, int32(defaultScaleDownCooldown), *behavior.ScaleDown.StabilizationWindowSeconds)
	assert.Equal(t, int32(1), behavior.ScaleUp.Policies[0].Value)
	assert.Equal(t, int32(1), behavior.ScaleDown.Policies[0].Value)
}
