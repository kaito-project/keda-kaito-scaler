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

// Package scaledobject owns the InferenceSet autoscaling annotation schema and
// builds the desired KEDA ScaledObject. Autoscaling is driven by the
// scaledobject.kaito.sh/metrics annotation, a YAML list of metric entries:
// configuring a single metric yields a single-signal ScaledObject, configuring
// several combines them under a conservative AND policy. The reconciler stays
// agnostic of how the ScaledObject is assembled by delegating to
// Builder.BuildDesired.
package scaledobject

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
)

const (
	// defaultPollingInterval is the interval (in seconds) at which KEDA polls
	// the external scaler for metrics. Overrides KEDA's built-in default of 30s
	// to give the autoscaler fresher signals for latency-sensitive inference
	// workloads.
	defaultPollingInterval = 15
)

// Auto-provision constants.
const (
	// Supported metricstype values.
	metricsTypeGauge     = "gauge"
	metricsTypeHistogram = "histogram"

	// combinePolicyAnd is the only supported combine policy: scale up only when
	// every metric is above its up-threshold, scale down only when every metric
	// is below its down-threshold.
	combinePolicyAnd = "AND"

	// Scaling multipliers and target drive the scalingModifiers formula, which
	// always evaluates to exactly one of the three multipliers below. KEDA feeds
	// that result as the metric value into HPA with metricType=Value and
	// scalingTarget, so HPA computes:
	//   desiredReplicas = ceil(currentReplicas * formulaOutput / scalingTarget)
	// With scalingTarget = 1 the formula output is exactly the desired-to-current
	// replica ratio: scale-up (>1) grows, scale-down (<1) shrinks, hold (=1)
	// keeps the count.
	//
	// The exact magnitudes are not significant: scaleUpMultiplier may be any
	// value > 1 and scaleDownMultiplier any value < 1 (each just has to sit
	// outside the tolerance band, see scalingTolerance). The HPA Pods policy
	// (value 1) then caps the actual change to +/-1 replica per cooldown
	// regardless of magnitude, giving conservative single-step scaling.
	scaleUpMultiplier   = "2.0"
	scaleDownMultiplier = "0.5"
	holdMultiplier      = "1.0"

	// scalingTarget is the scalingModifiers target. It must stay "1" so the
	// formula output maps directly to the desired-to-current replica ratio; it
	// also has to be > 0 for KEDA to create the scaler at all.
	scalingTarget = "1"

	// gateTriggerName is the reserved trigger/variable name for the readiness
	// gate in the scalingModifiers formula. Metric triggers instead use their
	// (sanitized) metric name as the variable name, so the formula reads in terms
	// of the real metrics rather than opaque indexed placeholders.
	gateTriggerName = "readiness_gate"

	// defaultQuantile is the target quantile applied to quantile-based metrics
	// when the per-metric quantile annotation is absent (p95).
	defaultQuantile = "0.95"

	// Defaults (seconds) when the corresponding annotation is absent.
	defaultEvaluationWindow  = 60
	defaultScaleUpCooldown   = 300
	defaultScaleDownCooldown = 300

	// scalingTolerance must be strictly below the scale-down multiplier's
	// distance from 1 (|0.5 - 1| = 0.5) so a 0.5 ratio still triggers a
	// scale-down, while a hold ratio of 1.0 is left untouched.
	scalingTolerance = "0.1"
)

// Builder carries the scaler service coordinates needed to build ScaledObject
// triggers that point back at the external scaler, and provides the
// auto-provision ScaledObject construction consumed by the reconciler.
type Builder struct {
	ScalerNamespace   string
	ScalerServiceName string
	ScalerGRPCPort    int
}

// BuildDesired validates the InferenceSet's auto-provision annotations
// and returns the desired ScaledObject, or an error when the configuration is
// invalid.
func (b Builder) BuildDesired(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int) (*v1alpha1.ScaledObject, error) {
	cfg, err := parseMetricsConfig(is.Annotations)
	if err != nil {
		return nil, err
	}
	return b.buildScaledObject(is, minReplicas, maxReplicas, cfg), nil
}

// combinePolicy expresses how per-metric conditions are combined into the
// scale-up and scale-down predicates of the scalingModifiers formula.
// Abstracting it behind an interface keeps buildFormula agnostic of the concrete
// policy and leaves room for future policies (e.g. OR) without touching the
// formula assembly.
type combinePolicy interface {
	// name returns the canonical policy name as used in the combinepolicy
	// annotation.
	name() string
	// scaleUpExpr builds the boolean predicate that must hold to scale up, given
	// the readiness-gate variable and each metric's up-threshold condition.
	scaleUpExpr(gateVar string, upConds []string) string
	// scaleDownExpr builds the boolean predicate that must hold to scale down.
	scaleDownExpr(downConds []string) string
}

// andCombinePolicy is the conservative policy: scale up only when the gate
// reports ready (== 1) AND every metric is above its up-threshold; scale down
// only when every metric is below its down-threshold.
type andCombinePolicy struct{}

func (andCombinePolicy) name() string { return combinePolicyAnd }

func (andCombinePolicy) scaleUpExpr(gateVar string, upConds []string) string {
	conds := append([]string{fmt.Sprintf("%s == 1", gateVar)}, upConds...)
	return strings.Join(conds, " && ")
}

func (andCombinePolicy) scaleDownExpr(downConds []string) string {
	return strings.Join(downConds, " && ")
}

// combinePolicies is the registry of supported combine policies keyed by name.
var combinePolicies = map[string]combinePolicy{
	combinePolicyAnd: andCombinePolicy{},
}

// metric is one parsed entry of an auto-provision configuration.
type metric struct {
	key string
	// source is the metric source name that produces this metric (from the metricsource
	// annotation; defaults to "modelpod").
	source string
	// aggregation is derived from the metricstype annotation: "service-avg" for
	// gauge metrics, "quantile" for histogram metrics.
	aggregation   string
	upThreshold   string
	downThreshold string
	// quantile is the target quantile (as a string, e.g. "0.95") for histogram
	// metrics using the "quantile" aggregation; empty for gauge metrics.
	quantile string
}

// aggregationForMetricsType maps a metricstype annotation value to the scaler
// aggregation used to reduce the scraped snapshot. metricstype is required, so an
// empty or unknown value is rejected.
func aggregationForMetricsType(t string) (string, error) {
	switch t {
	case metricsTypeGauge:
		return aggregator.ServiceAverageAggregatorName, nil
	case metricsTypeHistogram:
		return aggregator.QuantileAggregatorName, nil
	default:
		return "", fmt.Errorf("metricstype must be %q or %q, got %q", metricsTypeGauge, metricsTypeHistogram, t)
	}
}

// sourceForMetricSource validates the metricsource annotation value, defaulting
// to "modelpod" when empty. "modelpod" is currently the only supported source.
func sourceForMetricSource(s string) (string, error) {
	switch s {
	case "":
		return metricsource.ModelPodSourceName, nil
	case metricsource.ModelPodSourceName:
		return metricsource.ModelPodSourceName, nil
	default:
		return "", fmt.Errorf("metricsource must be %q, got %q", metricsource.ModelPodSourceName, s)
	}
}

// metricsConfig is the fully parsed auto-provision configuration.
type metricsConfig struct {
	metrics           []metric
	policy            combinePolicy
	evaluationWindow  int32
	scaleUpCooldown   int32
	scaleDownCooldown int32
}

// ValidateConfig validates the auto-provision annotations, returning an
// error describing the first invalid field. It lets the controller admit or
// reject an InferenceSet's configuration without exposing the internal parsed
// representation.
func ValidateConfig(annotations map[string]string) error {
	_, err := parseMetricsConfig(annotations)
	return err
}

// metricSpec is one entry of the scaledobject.kaito.sh/metrics annotation, a
// YAML (or JSON) list. Threshold and quantile fields are pointers so a missing
// key can be told apart from an explicit zero.
type metricSpec struct {
	// Name is the Prometheus metric name.
	Name string `json:"name"`
	// Type selects the aggregation: "gauge" or "histogram".
	Type string `json:"type"`
	// Source selects the metric source (optional, default "modelpod").
	Source string `json:"source,omitempty"`
	// UpThreshold is the scale-up threshold.
	UpThreshold *float64 `json:"upthreshold"`
	// DownThreshold is the scale-down threshold (must be <= upthreshold).
	DownThreshold *float64 `json:"downthreshold"`
	// Quantile is the target quantile in (0, 1] for histogram metrics (optional).
	Quantile *float64 `json:"quantile,omitempty"`
}

// parseMetricsConfig parses and validates the auto-provision annotations into a
// metricsConfig. The per-metric configuration is read from the
// scaledobject.kaito.sh/metrics annotation (a YAML list); the remaining global
// settings come from their own annotations. It returns an error describing the
// first invalid or missing field encountered.
func parseMetricsConfig(annotations map[string]string) (metricsConfig, error) {
	var cfg metricsConfig

	// Resolve the combine policy (default AND). Only registered policies are
	// accepted.
	policyName := combinePolicyAnd
	if p, ok := annotations[constants.AnnotationKeyCombinePolicy]; ok && p != "" {
		policyName = strings.ToUpper(p)
	}
	policy, ok := combinePolicies[policyName]
	if !ok {
		return cfg, fmt.Errorf("unsupported combinepolicy %q", annotations[constants.AnnotationKeyCombinePolicy])
	}
	cfg.policy = policy

	// The per-metric configuration lives in a single YAML/JSON list annotation.
	raw := annotations[constants.AnnotationKeyMetrics]
	if strings.TrimSpace(raw) == "" {
		return cfg, fmt.Errorf("auto-provision requires the %s annotation with at least one metric", constants.AnnotationKeyMetrics)
	}
	var specs []metricSpec
	if err := yaml.Unmarshal([]byte(raw), &specs); err != nil {
		return cfg, fmt.Errorf("invalid %s annotation: %w", constants.AnnotationKeyMetrics, err)
	}
	if len(specs) == 0 {
		return cfg, fmt.Errorf("auto-provision requires the %s annotation with at least one metric", constants.AnnotationKeyMetrics)
	}

	seenVars := make(map[string]string)
	for i, spec := range specs {
		if spec.Name == "" {
			return cfg, fmt.Errorf("metric index %d: name is required", i)
		}

		// type is required and decides the aggregation; source is optional and
		// selects the metric source (default "modelpod").
		aggregation, err := aggregationForMetricsType(spec.Type)
		if err != nil {
			return cfg, fmt.Errorf("metric %q (index %d): %w", spec.Name, i, err)
		}
		source, err := sourceForMetricSource(spec.Source)
		if err != nil {
			return cfg, fmt.Errorf("metric %q (index %d): %w", spec.Name, i, err)
		}

		// The metric name doubles as the formula variable/trigger name after
		// sanitization; two metrics collapsing to the same variable would make the
		// formula ambiguous and KEDA reject duplicate trigger names.
		varName := formulaVarName(spec.Name)
		if prev, dup := seenVars[varName]; dup {
			return cfg, fmt.Errorf("metric %q (index %d) collides with %q on formula variable %q", spec.Name, i, prev, varName)
		}
		seenVars[varName] = spec.Name

		if spec.UpThreshold == nil {
			return cfg, fmt.Errorf("metric %q (index %d): upthreshold is required", spec.Name, i)
		}
		if spec.DownThreshold == nil {
			return cfg, fmt.Errorf("metric %q (index %d): downthreshold is required", spec.Name, i)
		}
		up, down := *spec.UpThreshold, *spec.DownThreshold
		if down > up {
			return cfg, fmt.Errorf("metric %q (index %d): downthreshold (%s) must not exceed upthreshold (%s)", spec.Name, i, strconv.FormatFloat(down, 'f', -1, 64), strconv.FormatFloat(up, 'f', -1, 64))
		}

		// Quantile is only meaningful for histogram metrics (quantile aggregation);
		// default to p95 and validate the (0, 1] range when provided.
		quantile := ""
		if aggregation == aggregator.QuantileAggregatorName {
			quantile = defaultQuantile
			if spec.Quantile != nil {
				qv := *spec.Quantile
				if qv <= 0 || qv > 1 {
					return cfg, fmt.Errorf("metric %q (index %d): quantile must be in the (0, 1] range, got %s", spec.Name, i, strconv.FormatFloat(qv, 'f', -1, 64))
				}
				quantile = strconv.FormatFloat(qv, 'f', -1, 64)
			}
		}

		cfg.metrics = append(cfg.metrics, metric{
			key:           spec.Name,
			source:        source,
			aggregation:   aggregation,
			upThreshold:   strconv.FormatFloat(up, 'f', -1, 64),
			downThreshold: strconv.FormatFloat(down, 'f', -1, 64),
			quantile:      quantile,
		})
	}

	var err error
	if cfg.evaluationWindow, err = parseSecondsAnnotation(annotations, constants.AnnotationKeyEvaluationWindow, defaultEvaluationWindow); err != nil {
		return cfg, err
	}
	if cfg.scaleUpCooldown, err = parseSecondsAnnotation(annotations, constants.AnnotationKeyScaleUpCooldown, defaultScaleUpCooldown); err != nil {
		return cfg, err
	}
	if cfg.scaleDownCooldown, err = parseSecondsAnnotation(annotations, constants.AnnotationKeyScaleDownCooldown, defaultScaleDownCooldown); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// parseSecondsAnnotation parses a non-negative seconds value from the given
// annotation. It returns the default when the annotation is absent or empty, but
// returns an error when the annotation is present with an invalid value
// (non-integer, negative, or out of int32 range) so misconfiguration is surfaced
// instead of being silently ignored.
func parseSecondsAnnotation(annotations map[string]string, key string, def int32) (int32, error) {
	v, ok := annotations[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%s must be a non-negative integer within int32 range, got %q", key, v)
	}
	return int32(n), nil
}

// formulaVarName converts a metric name into a valid expr-lang identifier so it
// can be used as both the KEDA trigger name and the scalingModifiers formula
// variable. Any character that is not a letter, digit, or underscore is replaced
// with an underscore, and a leading digit is prefixed with one, ensuring names
// like "vllm:num_requests_waiting" become valid (e.g. "vllm_num_requests_waiting").
func formulaVarName(metricName string) string {
	var b strings.Builder
	for i, r := range metricName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteRune('_')
			}
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// buildFormula assembles the scalingModifiers expression implementing the
// conservative AND policy: scale up (multiplier 2.0) only when the readiness
// gate reports ready (== 1) AND every metric exceeds its up-threshold; scale
// down (multiplier 0.5) only when every metric is below its down-threshold;
// otherwise hold (1.0).
func buildFormula(cfg metricsConfig) string {
	upConds := make([]string, 0, len(cfg.metrics))
	downConds := make([]string, 0, len(cfg.metrics))
	for _, m := range cfg.metrics {
		v := formulaVarName(m.key)
		upConds = append(upConds, fmt.Sprintf("%s > %s", v, m.upThreshold))
		downConds = append(downConds, fmt.Sprintf("%s < %s", v, m.downThreshold))
	}
	return fmt.Sprintf("(%s) ? %s : ((%s) ? %s : %s)",
		cfg.policy.scaleUpExpr(gateTriggerName, upConds), scaleUpMultiplier,
		cfg.policy.scaleDownExpr(downConds), scaleDownMultiplier,
		holdMultiplier)
}

// buildTriggers builds one external trigger per metric (named after the
// sanitized metric name) plus the readiness gate trigger. All triggers use
// metricType Value; KEDA replaces their individual specs with a single composite
// spec derived from the scalingModifiers formula.
func (b Builder) buildTriggers(inferenceSetName, inferenceSetNamespace string, cfg metricsConfig) []v1alpha1.ScaleTriggers {
	scalerAddress := fmt.Sprintf("%s.%s.svc.cluster.local:%d", b.ScalerServiceName, b.ScalerNamespace, b.ScalerGRPCPort)
	authRef := &v1alpha1.AuthenticationRef{
		Name: constants.ClusterTriggerAuthName,
		Kind: constants.ClusterTriggerAuthKind,
	}

	triggers := make([]v1alpha1.ScaleTriggers, 0, len(cfg.metrics)+1)
	for _, m := range cfg.metrics {
		metadata := map[string]string{
			constants.InferenceSetNameInMetadata:      inferenceSetName,
			constants.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
			constants.ScalerAddressInMetadata:         scalerAddress,
			constants.MetricNameInMetadata:            m.key,
			constants.MetricSourceInMetadata:          m.source,
			constants.AggregationInMetadata:           m.aggregation,
		}
		if m.quantile != "" {
			metadata[constants.QuantileInMetadata] = m.quantile
		}
		triggers = append(triggers, v1alpha1.ScaleTriggers{
			Type:              "external",
			Name:              formulaVarName(m.key),
			Metadata:          metadata,
			AuthenticationRef: authRef,
			MetricType:        autoscalingv2.ValueMetricType,
		})
	}

	// Readiness gate trigger: reports 1 once all replicas are ready, 0 otherwise.
	triggers = append(triggers, v1alpha1.ScaleTriggers{
		Type: "external",
		Name: gateTriggerName,
		Metadata: map[string]string{
			constants.InferenceSetNameInMetadata:      inferenceSetName,
			constants.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
			constants.ScalerAddressInMetadata:         scalerAddress,
			constants.MetricNameInMetadata:            gateTriggerName,
			constants.AggregationInMetadata:           constants.AggregationGate,
		},
		AuthenticationRef: authRef,
		MetricType:        autoscalingv2.ValueMetricType,
	})

	return triggers
}

// buildHPAConfig builds the HPA behaviour for auto-provision: the
// evaluation window gates scale-up stabilization, the cooldowns bound how often
// replicas may change, and the tolerance is tightened to 0.1 in both directions
// so the 0.5 scale-down ratio is not swallowed.
func buildHPAConfig(cfg metricsConfig) *v1alpha1.HorizontalPodAutoscalerConfig {
	tolerance := func() *resource.Quantity {
		q := resource.MustParse(scalingTolerance)
		return &q
	}
	return &v1alpha1.HorizontalPodAutoscalerConfig{
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(cfg.evaluationWindow),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: cfg.scaleUpCooldown,
					},
				},
				Tolerance: tolerance(),
			},
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(cfg.scaleDownCooldown),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: cfg.scaleDownCooldown,
					},
				},
				Tolerance: tolerance(),
			},
		},
	}
}

func (b Builder) buildScaledObject(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int, cfg metricsConfig) *v1alpha1.ScaledObject {
	return &v1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      is.Name,
			Namespace: is.Namespace,
			Annotations: map[string]string{
				constants.AnnotationKeyManagedBy: scaler.ScalerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         constants.InferenceSetAPIVersion,
					Kind:               constants.InferenceSet,
					Name:               is.Name,
					UID:                is.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.ScaledObjectSpec{
			Advanced: &v1alpha1.AdvancedConfig{
				HorizontalPodAutoscalerConfig: buildHPAConfig(cfg),
				ScalingModifiers: v1alpha1.ScalingModifiers{
					Formula:    buildFormula(cfg),
					Target:     scalingTarget,
					MetricType: autoscalingv2.ValueMetricType,
				},
			},
			ScaleTargetRef: &v1alpha1.ScaleTarget{
				Name:       is.Name,
				APIVersion: constants.InferenceSetAPIVersion,
				Kind:       constants.InferenceSet,
			},
			PollingInterval: ptr.To(int32(defaultPollingInterval)),
			MinReplicaCount: ptr.To(int32(minReplicas)),
			MaxReplicaCount: ptr.To(int32(maxReplicas)),
			Triggers:        b.buildTriggers(is.Name, is.Namespace, cfg),
		},
	}
}
