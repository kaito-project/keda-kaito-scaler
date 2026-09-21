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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
)

// metricsAnn wraps a metrics YAML list body into an annotation map.
func metricsAnn(body string) map[string]string {
	return map[string]string{constants.AnnotationKeyMetrics: body}
}

// metricsAnnotations returns a valid two-metric annotation set.
func metricsAnnotations() map[string]string {
	return metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 10
  downthreshold: 2
- name: vllm:request_queue_time_seconds
  type: histogram
  upthreshold: 1.5
  downthreshold: 0.5
`)
}

func TestParseMetricsConfig(t *testing.T) {
	t.Run("valid two-metric config", func(t *testing.T) {
		cfg, err := parseMetricsConfig(metricsAnnotations(), 1)
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 2)
		assert.Equal(t, "vllm:num_requests_waiting", cfg.metrics[0].key)
		assert.Equal(t, "modelpod", cfg.metrics[0].source)
		assert.Equal(t, "service-avg", cfg.metrics[0].aggregation)
		assert.Equal(t, "10", cfg.metrics[0].upThreshold)
		assert.Equal(t, "2", cfg.metrics[0].downThreshold)
		assert.Equal(t, "vllm:request_queue_time_seconds", cfg.metrics[1].key)
		assert.Equal(t, "windowed-avg", cfg.metrics[1].aggregation)
		// Metric cache window defaults to 300 seconds for histogram metrics.
		assert.Equal(t, "300", cfg.metrics[1].metricCacheWindow)
		// Defaults applied.
		assert.Equal(t, int32(defaultEvaluationWindow), cfg.evaluationWindow)
		assert.Equal(t, int32(defaultScaleUpCooldown), cfg.scaleUpCooldown)
		assert.Equal(t, int32(defaultScaleDownCooldown), cfg.scaleDownCooldown)
	})

	t.Run("valid single-metric config", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 5
  downthreshold: 1
`)
		cfg, err := parseMetricsConfig(ann, 1)
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 1)
		assert.Equal(t, "service-avg", cfg.metrics[0].aggregation)
	})

	t.Run("custom windows and cooldowns", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyEvaluationWindow] = "30"
		ann[constants.AnnotationKeyScaleUpCooldown] = "120"
		ann[constants.AnnotationKeyScaleDownCooldown] = "240"
		cfg, err := parseMetricsConfig(ann, 1)
		assert.NoError(t, err)
		assert.Equal(t, int32(30), cfg.evaluationWindow)
		assert.Equal(t, int32(120), cfg.scaleUpCooldown)
		assert.Equal(t, int32(240), cfg.scaleDownCooldown)
	})

	t.Run("missing metricstype rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  upthreshold: 10
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("invalid metricstype rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: bogus
  upthreshold: 10
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("invalid metricsource rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  source: bogus
  upthreshold: 10
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("invalid up threshold rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: abc
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("non-finite thresholds rejected", func(t *testing.T) {
		for _, v := range []string{"NaN", ".inf", "+.inf", "-.inf"} {
			ann := metricsAnn(fmt.Sprintf(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: %s
  downthreshold: 2
`, v))
			_, err := parseMetricsConfig(ann, 1)
			assert.Error(t, err, "upthreshold %q", v)

			ann = metricsAnn(fmt.Sprintf(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 2
  downthreshold: %s
`, v))
			_, err = parseMetricsConfig(ann, 1)
			assert.Error(t, err, "downthreshold %q", v)
		}
	})

	t.Run("down greater than up rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 10
  downthreshold: 20
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("custom metriccachewindow accepted", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:request_queue_time_seconds
  type: histogram
  upthreshold: 1.5
  downthreshold: 0.5
  metriccachewindow: 120
`)
		cfg, err := parseMetricsConfig(ann, 1)
		assert.NoError(t, err)
		assert.Equal(t, "120", cfg.metrics[0].metricCacheWindow)
	})

	t.Run("non-positive metriccachewindow rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:request_queue_time_seconds
  type: histogram
  upthreshold: 1.5
  downthreshold: 0.5
  metriccachewindow: 0
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("metriccachewindow on gauge rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_waiting
  type: gauge
  upthreshold: 5
  downthreshold: 1
  metriccachewindow: 60
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("non-AND combine policy rejected", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyCombinePolicy] = "OR"
		_, err := parseMetricsConfig(ann, 1)
		assert.Error(t, err)
	})

	t.Run("explicit AND accepted", func(t *testing.T) {
		ann := metricsAnnotations()
		ann[constants.AnnotationKeyCombinePolicy] = "AND"
		_, err := parseMetricsConfig(ann, 1)
		assert.NoError(t, err)
	})

	t.Run("no metrics rejected", func(t *testing.T) {
		_, err := parseMetricsConfig(map[string]string{constants.AnnotationKeyAutoProvision: "true"}, 1)
		assert.Error(t, err)
	})
}

func TestBuildFormula(t *testing.T) {
	cfg, err := parseMetricsConfig(metricsAnnotations(), 1)
	assert.NoError(t, err)
	got := buildFormula(cfg, 1, 5)
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

	cfg, err := parseMetricsConfig(metricsAnnotations(), 1)
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

	// metric_1 -> vllm:request_queue_time_seconds via modelpod + windowed-avg, carrying the default 300s cache window.
	m1 := so.Spec.Triggers[1].Metadata
	assert.Equal(t, "windowed-avg", m1[constants.AggregationInMetadata])
	assert.Equal(t, "300", m1[constants.MetricCacheWindowInMetadata])

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
	assert.Equal(t, buildFormula(cfg, 1, 5), sm.Formula)

	// HPA behaviour tolerances tightened to 0.1 in both directions.
	behavior := so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior
	assert.InDelta(t, 0.1, behavior.ScaleUp.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 0.1, behavior.ScaleDown.Tolerance.AsApproximateFloat64(), 1e-9)
	assert.Equal(t, int32(defaultEvaluationWindow), *behavior.ScaleUp.StabilizationWindowSeconds)
	assert.Equal(t, int32(defaultScaleDownCooldown), *behavior.ScaleDown.StabilizationWindowSeconds)
	assert.Equal(t, int32(1), behavior.ScaleUp.Policies[0].Value)
	assert.Equal(t, int32(1), behavior.ScaleDown.Policies[0].Value)
}

// scaleToZeroAnnotations returns a minimal configuration that satisfies every
// scale-to-zero rule: a paired EPP-observed activation/deactivation signal so
// queued work can wake and hold the workload, a backend-observed deactivation
// signal so it can be parked safely, and an up/down band for the 1..N range.
func scaleToZeroAnnotations() map[string]string {
	return metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0}
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
  upthreshold: 10
  downthreshold: 2
`)
}

func TestParseMetricsConfig_ScaleToZero(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		cfg, err := parseMetricsConfig(scaleToZeroAnnotations(), 0)
		assert.NoError(t, err)
		assert.Len(t, cfg.metrics, 2)

		// An EPP gauge is summed across router replicas rather than averaged.
		assert.Equal(t, "epp", cfg.metrics[0].source)
		assert.Equal(t, "sum", cfg.metrics[0].aggregation)
		assert.Equal(t, "0", cfg.metrics[0].activationThreshold)
		assert.Equal(t, "0", cfg.metrics[0].deactivationThreshold)
		assert.False(t, cfg.metrics[0].hasUpDownBand())

		assert.Equal(t, "modelpod", cfg.metrics[1].source)
		assert.Equal(t, "0", cfg.metrics[1].deactivationThreshold)
		assert.True(t, cfg.metrics[1].hasUpDownBand())
	})

	// The up/down band is what drives the 1..N range, so it stays mandatory
	// whenever the minimum is not 0. Relaxing it for scale-to-zero must not
	// relax it for the existing configurations.
	t.Run("up and down stay required when the minimum is not zero", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_running
  type: gauge
  upthreshold: 10
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.ErrorContains(t, err, "downthreshold is required")

		ann = metricsAnn(`
- name: vllm:num_requests_running
  type: gauge
  downthreshold: 2
`)
		_, err = parseMetricsConfig(ann, 1)
		assert.ErrorContains(t, err, "upthreshold is required")
	})

	// Activation and deactivation only mean anything on the 0 <-> 1 edge, so
	// accepting them at a non-zero minimum would silently ignore them.
	t.Run("activation thresholds are rejected when the minimum is not zero", func(t *testing.T) {
		ann := metricsAnn(`
- name: inference_pool_per_pod_queue_size
  type: gauge
  source: epp
  upthreshold: 10
  downthreshold: 2
  activationthreshold: 0
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.ErrorContains(t, err, constants.AnnotationKeyMinReplicas)
		assert.ErrorContains(t, err, "activationthreshold")
	})

	t.Run("a metric may omit the up and down band when the minimum is zero", func(t *testing.T) {
		cfg, err := parseMetricsConfig(scaleToZeroAnnotations(), 0)
		assert.NoError(t, err)
		assert.Empty(t, cfg.metrics[0].upThreshold)
		assert.Empty(t, cfg.metrics[0].downThreshold)
	})

	t.Run("activation requires an explicit deactivation threshold", func(t *testing.T) {
		ann := metricsAnn(`
- name: inference_pool_per_pod_queue_size
  type: gauge
  source: epp
  activationthreshold: 0
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "activationthreshold requires deactivationthreshold on the same metric")
	})

	// A half-declared band would compare against a threshold the other
	// direction never sets, so it is rejected in both modes.
	t.Run("a partial up/down band is always rejected", func(t *testing.T) {
		ann := metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0, upthreshold: 10}
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "must be declared together")
	})

	// Only the EPP keeps reporting while the workload is parked, so an
	// activation threshold on a backend metric could never be observed.
	t.Run("activation requires a source observable at zero replicas", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_running
  type: gauge
  activationthreshold: 0
  deactivationthreshold: 0
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "observable at zero replicas")
	})

	t.Run("a config that can never wake is rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
  upthreshold: 10
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "activationthreshold")
	})

	// Parking on the router's view alone could stop a replica that is still
	// generating tokens for an in-flight request.
	t.Run("a config that parks without observing the backend is rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: inference_pool_per_pod_queue_size
  type: gauge
  source: epp
  activationthreshold: 0
  deactivationthreshold: 0
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "backend occupancy")
	})

	t.Run("deactivation must not exceed activation", func(t *testing.T) {
		ann := metricsAnn(`
- name: inference_pool_per_pod_queue_size
  type: gauge
  source: epp
  activationthreshold: 1
  deactivationthreshold: 5
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "must not exceed")
	})

	// replica_count is a synthetic trigger the formula branches on, so a user
	// metric of the same name would shadow it.
	t.Run("the replica_count name is reserved when the minimum is zero", func(t *testing.T) {
		ann := metricsAnn(`
- {name: replica_count, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0}
`)
		_, err := parseMetricsConfig(ann, 0)
		assert.ErrorContains(t, err, "reserved")

		// The trigger does not exist at a non-zero minimum, so the same name
		// stays usable there and existing configurations keep working.
		ann = metricsAnn(`
- name: replica_count
  type: gauge
  upthreshold: 10
  downthreshold: 2
`)
		_, err = parseMetricsConfig(ann, 1)
		assert.NoError(t, err)
	})

	t.Run("cooldownperiod", func(t *testing.T) {
		ann := scaleToZeroAnnotations()
		ann[constants.AnnotationKeyCooldownPeriod] = "60"
		cfg, err := parseMetricsConfig(ann, 0)
		assert.NoError(t, err)
		assert.Equal(t, int32(60), *cfg.cooldownPeriod)

		// It only governs the wait before parking, so it is meaningless when
		// the workload never reaches zero.
		ann = metricsAnnotations()
		ann[constants.AnnotationKeyCooldownPeriod] = "60"
		_, err = parseMetricsConfig(ann, 1)
		assert.ErrorContains(t, err, constants.AnnotationKeyMinReplicas)
	})

	t.Run("an explicit aggregation override is honoured", func(t *testing.T) {
		ann := metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, aggregation: service-avg, activationthreshold: 0, deactivationthreshold: 0}
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
  upthreshold: 10
  downthreshold: 2
`)
		cfg, err := parseMetricsConfig(ann, 0)
		assert.NoError(t, err)
		assert.Equal(t, "service-avg", cfg.metrics[0].aggregation)
	})

	// windowed-avg reads a histogram's _sum/_count pair, so pointing it at a
	// gauge would read fields the source never populates.
	t.Run("an aggregation incompatible with the metric type is rejected", func(t *testing.T) {
		ann := metricsAnn(`
- name: vllm:num_requests_running
  type: gauge
  aggregation: windowed-avg
  upthreshold: 10
  downthreshold: 2
`)
		_, err := parseMetricsConfig(ann, 1)
		assert.ErrorContains(t, err, "not compatible")
	})
}

func TestBuildScaleToZeroFormula(t *testing.T) {
	t.Run("0..N branches on replica_count then on the band", func(t *testing.T) {
		cfg, err := parseMetricsConfig(scaleToZeroAnnotations(), 0)
		assert.NoError(t, err)

		got := buildFormula(cfg, 0, 5)
		want := "(replica_count == 0) ? " +
			"((inference_pool_per_pod_queue_size > 0) ? 1.0 : 0.0) : " +
			"((inference_pool_per_pod_queue_size <= 0 && vllm_num_requests_running <= 0) ? 0.0 : " +
			"((readiness_gate == 1 && vllm_num_requests_running > 10) ? 2.0 : " +
			"((vllm_num_requests_running < 2) ? 0.5 : 1.0)))"
		assert.Equal(t, want, got)
	})

	t.Run("0..1 collapses to deactivate or hold", func(t *testing.T) {
		cfg, err := parseMetricsConfig(metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0}
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
`), 0)
		assert.NoError(t, err)

		got := buildFormula(cfg, 0, 1)
		want := "(replica_count == 0) ? " +
			"((inference_pool_per_pod_queue_size > 0) ? 1.0 : 0.0) : " +
			"((inference_pool_per_pod_queue_size <= 0 && vllm_num_requests_running <= 0) ? 0.0 : 1.0)"
		assert.Equal(t, want, got)
	})

	t.Run("0..N without an up-down band leaves the 1 to N range inert", func(t *testing.T) {
		cfg, err := parseMetricsConfig(metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0}
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
`), 0)
		assert.NoError(t, err)

		got := buildFormula(cfg, 0, 5)
		want := "(replica_count == 0) ? " +
			"((inference_pool_per_pod_queue_size > 0) ? 1.0 : 0.0) : " +
			"((inference_pool_per_pod_queue_size <= 0 && vllm_num_requests_running <= 0) ? 0.0 : 1.0)"
		assert.Equal(t, want, got)
	})

	// Any one signal crossing its threshold should wake the workload, while
	// every signal must agree before it is parked.
	t.Run("activation ORs and deactivation ANDs", func(t *testing.T) {
		cfg, err := parseMetricsConfig(metricsAnn(`
- {name: inference_pool_per_pod_queue_size, type: gauge, source: epp, activationthreshold: 0, deactivationthreshold: 0}
- {name: kv_cache_utilization, type: gauge, source: epp, activationthreshold: 1, deactivationthreshold: 1}
- name: vllm:num_requests_running
  type: gauge
  deactivationthreshold: 0
`), 0)
		assert.NoError(t, err)

		got := buildFormula(cfg, 0, 1)
		assert.Contains(t, got, "(inference_pool_per_pod_queue_size > 0 || kv_cache_utilization > 1) ? 1.0 : 0.0")
		assert.Contains(t, got, "(inference_pool_per_pod_queue_size <= 0 && kv_cache_utilization <= 1 && vllm_num_requests_running <= 0) ? 0.0 : 1.0")
	})
}

// An empty predicate must never collapse into something that fires. expr-lang
// reads an empty AND as vacuously true, which would park or shrink the workload
// on every evaluation.
func TestJoinPredicate(t *testing.T) {
	assert.Equal(t, "false", joinPredicate(nil, "&&"))
	assert.Equal(t, "false", joinPredicate([]string{}, "||"))
	assert.Equal(t, "a > 1", joinPredicate([]string{"a > 1"}, "&&"))
	assert.Equal(t, "a > 1 && b < 2", joinPredicate([]string{"a > 1", "b < 2"}, "&&"))
}

func TestBuildScaledObject_ScaleToZero(t *testing.T) {
	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is", Namespace: "default", UID: "uid"},
	}
	b := Builder{ScalerNamespace: "kaito-workspace", ScalerServiceName: "kaito-scaler", ScalerGRPCPort: 9443}

	ann := scaleToZeroAnnotations()
	ann[constants.AnnotationKeyCooldownPeriod] = "60"
	cfg, err := parseMetricsConfig(ann, 0)
	assert.NoError(t, err)

	so := b.buildScaledObject(is, 0, 5, cfg)

	assert.Equal(t, int32(0), *so.Spec.MinReplicaCount)
	assert.Equal(t, int32(5), *so.Spec.MaxReplicaCount)
	assert.Equal(t, int32(60), *so.Spec.CooldownPeriod)

	// KEDA compares the composite value against activationTarget to decide
	// 0 -> 1, so it has to be 0 for the activation branch's 1.0 to wake it.
	assert.Equal(t, "0", so.Spec.Advanced.ScalingModifiers.ActivationTarget)

	// One trigger per metric, plus the readiness gate and the replica_count
	// selector the formula branches on.
	byName := map[string]map[string]string{}
	for _, tr := range so.Spec.Triggers {
		byName[tr.Name] = tr.Metadata
	}
	assert.Len(t, so.Spec.Triggers, 4)
	assert.Contains(t, byName, "replica_count")
	assert.Contains(t, byName, "readiness_gate")
	assert.Equal(t, constants.AggregationReplicas, byName["replica_count"][constants.AggregationInMetadata])

	// The EPP exposes its own Prometheus endpoint, so the trigger cannot rely
	// on the scaler's Service-oriented defaults.
	epp := byName["inference_pool_per_pod_queue_size"]
	assert.Equal(t, "epp", epp[constants.MetricSourceInMetadata])
	assert.Equal(t, "sum", epp[constants.AggregationInMetadata])
	assert.NotContains(t, epp, constants.ThresholdInMetadata)
	assert.Equal(t, "9090", epp[constants.MetricPortInMetadata])
	assert.Equal(t, "/metrics", epp[constants.MetricPathInMetadata])
	assert.NotContains(t, epp, constants.ZeroReplicaFallbackInMetadata)

	// A backend metric cannot be scraped while parked, so it opts into
	// reporting 0 rather than erroring on every poll.
	backend := byName["vllm_num_requests_running"]
	assert.Equal(t, "modelpod", backend[constants.MetricSourceInMetadata])
	assert.Equal(t, "true", backend[constants.ZeroReplicaFallbackInMetadata])
}

// The scale-to-zero work must be inert for the configurations already running.
func TestBuildScaledObject_NonZeroMinimumUnchanged(t *testing.T) {
	is := &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is", Namespace: "default", UID: "uid"},
	}
	b := Builder{ScalerNamespace: "kaito-workspace", ScalerServiceName: "kaito-scaler", ScalerGRPCPort: 9443}

	cfg, err := parseMetricsConfig(metricsAnnotations(), 1)
	assert.NoError(t, err)
	so := b.buildScaledObject(is, 1, 5, cfg)

	assert.Nil(t, so.Spec.CooldownPeriod)
	assert.Empty(t, so.Spec.Advanced.ScalingModifiers.ActivationTarget)
	assert.Len(t, so.Spec.Triggers, 3)
	for _, tr := range so.Spec.Triggers {
		assert.NotEqual(t, "replica_count", tr.Name)
		assert.NotContains(t, tr.Metadata, constants.ZeroReplicaFallbackInMetadata)
	}
	assert.NotContains(t, so.Spec.Advanced.ScalingModifiers.Formula, "replica_count")
}

func TestBuildDesired_EqualPositiveReplicaBounds(t *testing.T) {
	b := Builder{ScalerNamespace: "kaito-workspace", ScalerServiceName: "kaito-scaler", ScalerGRPCPort: 9443}

	for _, replicas := range []int{1, 2} {
		t.Run(fmt.Sprintf("fixed at %d", replicas), func(t *testing.T) {
			is := &kaitov1beta1.InferenceSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-is",
					Namespace:   "default",
					UID:         "uid",
					Annotations: metricsAnnotations(),
				},
			}

			so, err := b.BuildDesired(is, replicas, replicas)
			assert.NoError(t, err)
			assert.Equal(t, int32(replicas), *so.Spec.MinReplicaCount)
			assert.Equal(t, int32(replicas), *so.Spec.MaxReplicaCount)
			assert.Equal(t,
				"(readiness_gate == 1 && vllm_num_requests_waiting > 10 && vllm_request_queue_time_seconds > 1.5) ? 2.0 : ((vllm_num_requests_waiting < 2 && vllm_request_queue_time_seconds < 0.5) ? 0.5 : 1.0)",
				so.Spec.Advanced.ScalingModifiers.Formula)
		})
	}
}
