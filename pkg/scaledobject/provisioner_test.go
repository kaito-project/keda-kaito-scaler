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
	"testing"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
)

// metricsAnnotations returns a valid two-metric annotation set.
func metricsAnnotations() map[string]string {
	return map[string]string{
		indexedKey(constants.AnnotationKeyMetricName, 0): "vllm:num_requests_waiting",
		"scaledobject.kaito.sh/metricstype/0":            "gauge",
		"scaledobject.kaito.sh/upthreshold/0":            "10",
		"scaledobject.kaito.sh/downthreshold/0":          "2",
		indexedKey(constants.AnnotationKeyMetricName, 1): "vllm:request_queue_time_seconds",
		"scaledobject.kaito.sh/metricstype/1":            "histogram",
		"scaledobject.kaito.sh/upthreshold/1":            "1.5",
		"scaledobject.kaito.sh/downthreshold/1":          "0.5",
	}
}

func TestParseMetricsConfig(t *testing.T) {
	t.Run("valid two-metric config", func(t *testing.T) {
		cfg, err := parseMetricsConfig(metricsAnnotations())
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 2)
		assert.Equal(t, "vllm:num_requests_waiting", cfg.metrics[0].key)
		assert.Equal(t, "modelpod", cfg.metrics[0].source)
		assert.Equal(t, "service-avg", cfg.metrics[0].aggregation)
		assert.Equal(t, "10", cfg.metrics[0].upThreshold)
		assert.Equal(t, "2", cfg.metrics[0].downThreshold)
		assert.Equal(t, "vllm:request_queue_time_seconds", cfg.metrics[1].key)
		assert.Equal(t, "quantile", cfg.metrics[1].aggregation)
		// Quantile defaults to p95 for quantile-aggregated metrics.
		assert.Equal(t, "0.95", cfg.metrics[1].quantile)
		// Defaults applied.
		assert.Equal(t, int32(defaultEvaluationWindow), cfg.evaluationWindow)
		assert.Equal(t, int32(defaultScaleUpCooldown), cfg.scaleUpCooldown)
		assert.Equal(t, int32(defaultScaleDownCooldown), cfg.scaleDownCooldown)
	})

	t.Run("valid single-metric config", func(t *testing.T) {
		ann := map[string]string{
			indexedKey(constants.AnnotationKeyMetricName, 0): "vllm:num_requests_waiting",
			"scaledobject.kaito.sh/metricstype/0":            "gauge",
			"scaledobject.kaito.sh/upthreshold/0":            "5",
			"scaledobject.kaito.sh/downthreshold/0":          "1",
		}
		cfg, err := parseMetricsConfig(ann)
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 1)
		assert.Equal(t, "service-avg", cfg.metrics[0].aggregation)
	})

	t.Run("custom windows and cooldowns", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyEvaluationWindow] = "30"
		ann[constants.AnnotationKeyScaleUpCooldown] = "120"
		ann[constants.AnnotationKeyScaleDownCooldown] = "240"
		cfg, err := parseMetricsConfig(ann)
		assert.NoError(t, err)
		assert.Equal(t, int32(30), cfg.evaluationWindow)
		assert.Equal(t, int32(120), cfg.scaleUpCooldown)
		assert.Equal(t, int32(240), cfg.scaleDownCooldown)
	})

	t.Run("missing metricstype rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		delete(ann, "scaledobject.kaito.sh/metricstype/0")
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("invalid metricstype rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/metricstype/0"] = "bogus"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("invalid metricsource rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/metricsource/0"] = "bogus"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("invalid up threshold rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/upthreshold/0"] = "abc"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("non-finite thresholds rejected", func(t *testing.T) {
		for _, v := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
			ann := metricsAnnotations()
			ann["scaledobject.kaito.sh/upthreshold/0"] = v
			_, err := parseMetricsConfig(ann)
			assert.Error(t, err, "upthreshold %q", v)

			ann = metricsAnnotations()
			ann["scaledobject.kaito.sh/downthreshold/0"] = v
			_, err = parseMetricsConfig(ann)
			assert.Error(t, err, "downthreshold %q", v)
		}
	})

	t.Run("down greater than up rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/downthreshold/0"] = "20"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("NaN quantile rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[indexedKey(constants.AnnotationKeyQuantile, 1)] = "NaN"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("non-AND combine policy rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyCombinePolicy] = "OR"
		_, err := parseMetricsConfig(ann)
		assert.Error(t, err)
	})

	t.Run("explicit AND accepted", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyCombinePolicy] = "AND"
		_, err := parseMetricsConfig(ann)
		assert.NoError(t, err)
	})

	t.Run("no metrics rejected", func(t *testing.T) {
		_, err := parseMetricsConfig(map[string]string{"scaledobject.kaito.sh/auto-provision": "true"})
		assert.Error(t, err)
	})
}

func TestBuildFormula(t *testing.T) {
	cfg, err := parseMetricsConfig(metricsAnnotations())
	assert.NoError(t, err)
	got := buildFormula(cfg)
	want := "(readiness_gate == 1 && vllm_num_requests_waiting > 10 && vllm_request_queue_time_seconds > 1.5) ? 2.0 : ((vllm_num_requests_waiting < 2 && vllm_request_queue_time_seconds < 0.5) ? 0.5 : 1.0)"
	assert.Equal(t, want, got)
}

func TestBuildScaledObject(t *testing.T) {
	const (
		isName = "test-is"
		ns     = "default"
	)
	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns, UID: "uid"},
	}
	b := Builder{ScalerNamespace: "kaito-workspace", ScalerServiceName: "kaito-scaler", ScalerGRPCPort: 9443}

	cfg, err := parseMetricsConfig(metricsAnnotations())
	assert.NoError(t, err)

	so := b.buildScaledObject(is, 1, 5, cfg)

	// Object meta / target.
	assert.Equal(t, isName, so.Name)
	assert.Equal(t, ns, so.Namespace)
	assert.Equal(t, int32(defaultPollingInterval), *so.Spec.PollingInterval)
	assert.Equal(t, int32(1), *so.Spec.MinReplicaCount)
	assert.Equal(t, int32(5), *so.Spec.MaxReplicaCount)
	assert.Equal(t, isName, so.Spec.ScaleTargetRef.Name)

	// One trigger per metric plus the readiness gate.
	assert.Len(t, so.Spec.Triggers, 3)
	assert.Equal(t, "vllm_num_requests_waiting", so.Spec.Triggers[0].Name)
	assert.Equal(t, "vllm_request_queue_time_seconds", so.Spec.Triggers[1].Name)
	assert.Equal(t, "readiness_gate", so.Spec.Triggers[2].Name)

	// metric_0 -> vllm:num_requests_waiting via modelpod + service-avg.
	m0 := so.Spec.Triggers[0].Metadata
	assert.Equal(t, "modelpod", m0[constants.MetricSourceInMetadata])
	assert.Equal(t, "service-avg", m0[constants.AggregationInMetadata])
	assert.Equal(t, "vllm:num_requests_waiting", m0[constants.MetricNameInMetadata])
	assert.NotContains(t, m0, "threshold", "triggers omit the per-replica threshold")

	// metric_1 -> vllm:request_queue_time_seconds via modelpod + quantile, carrying the default p95 quantile.
	m1 := so.Spec.Triggers[1].Metadata
	assert.Equal(t, "quantile", m1[constants.AggregationInMetadata])
	assert.Equal(t, "0.95", m1[constants.QuantileInMetadata])

	// gate trigger uses the gate aggregation.
	gate := so.Spec.Triggers[2].Metadata
	assert.Equal(t, constants.AggregationGate, gate[constants.AggregationInMetadata])

	// All triggers use Value metric type.
	for _, tr := range so.Spec.Triggers {
		assert.Equal(t, autoscalingv2.ValueMetricType, tr.MetricType)
		assert.Equal(t, constants.ClusterTriggerAuthName, tr.AuthenticationRef.Name)
	}

	// scalingModifiers wired with target 1 and Value metric type.
	sm := so.Spec.Advanced.ScalingModifiers
	assert.Equal(t, "1", sm.Target)
	assert.Equal(t, autoscalingv2.ValueMetricType, sm.MetricType)
	assert.Equal(t, buildFormula(cfg), sm.Formula)

	// HPA behaviour tolerances tightened to 0.1 in both directions.
	behavior := so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior
	assert.InDelta(t, 0.1, behavior.ScaleUp.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 0.1, behavior.ScaleDown.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.Equal(t, int32(defaultEvaluationWindow), *behavior.ScaleUp.StabilizationWindowSeconds)
	assert.Equal(t, int32(defaultScaleDownCooldown), *behavior.ScaleDown.StabilizationWindowSeconds)
	assert.Equal(t, int32(1), behavior.ScaleUp.Policies[0].Value)
	assert.Equal(t, int32(1), behavior.ScaleDown.Policies[0].Value)
}
