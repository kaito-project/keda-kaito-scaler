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

	// activateMultiplier and deactivateMultiplier drive the 0 <-> 1 transitions.
	// KEDA evaluates those against ScalingModifiers.ActivationTarget on the
	// scaleFromZeroOrIdle path, which bypasses the HPA entirely: only "strictly
	// greater than the target" is compared, so the magnitude is discarded and
	// 1.0 is used rather than scaleUpMultiplier.
	activateMultiplier   = "1.0"
	deactivateMultiplier = "0.0"

	// scalingTarget is the scalingModifiers target. It must stay "1" so the
	// formula output maps directly to the desired-to-current replica ratio; it
	// also has to be > 0 for KEDA to create the scaler at all.
	scalingTarget = "1"

	// activationTarget is the scalingModifiers activation target. The formula's
	// activate/deactivate multipliers are compared against it on KEDA's
	// scale-from-zero path, so it must stay "0": activateMultiplier is strictly
	// above it and deactivateMultiplier is equal to it.
	activationTarget = "0"

	// gateTriggerName is the reserved trigger/variable name for the readiness
	// gate in the scalingModifiers formula. Metric triggers instead use their
	// (sanitized) metric name as the variable name, so the formula reads in terms
	// of the real metrics rather than opaque indexed placeholders.
	gateTriggerName = "readiness_gate"

	// replicaCountTriggerName is the reserved trigger/variable name carrying the
	// InferenceSet's desired replica count. A scale-to-zero formula needs it to
	// tell the 0 -> 1 branch apart from the 1 <-> N branches, since the formula
	// itself has no other view of the current scale.
	replicaCountTriggerName = "replica_count"

	// defaultMetricCacheWindow is the cache window (seconds) applied to a
	// histogram metric when its per-metric metriccachewindow field is absent.
	defaultMetricCacheWindow = 300

	// eppMetricsPort and eppMetricsPath address the Endpoint Picker's Prometheus
	// endpoint. Emitted explicitly on epp triggers because the scaler's defaults
	// describe the workspace Service instead.
	eppMetricsPort = "9090"
	eppMetricsPath = "/metrics"

	// Defaults (seconds) when the corresponding annotation is absent.
	defaultEvaluationWindow  = 60
	defaultScaleUpCooldown   = 300
	defaultScaleDownCooldown = 300

	// defaultCooldownPeriod is KEDA's own default for the delay before scaling
	// to zero. Set explicitly so the rendered ScaledObject states the value it
	// is actually running with.
	defaultCooldownPeriod = 300

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
	cfg, err := parseMetricsConfig(is.Annotations, minReplicas)
	if err != nil {
		return nil, err
	}
	if err := validateReplicaRange(cfg, minReplicas, maxReplicas); err != nil {
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
	// aggregation is derived from the metricstype annotation ("service-avg" for
	// gauge metrics, "windowed-avg" for histogram metrics) unless the entry
	// overrides it explicitly.
	aggregation   string
	upThreshold   string
	downThreshold string
	// activationThreshold and deactivationThreshold drive the 0 -> 1 and 1 -> 0
	// branches respectively. Both are empty unless the ScaledObject's minimum is
	// 0, where at least one metric must supply each side.
	activationThreshold   string
	deactivationThreshold string
	// metricCacheWindow is the cache window in seconds (as a string, e.g. "300")
	// for histogram metrics using the windowed-avg aggregation; empty for gauge
	// metrics.
	metricCacheWindow string
}

// hasUpDownBand reports whether the entry participates in the 1 <-> N range.
func (m metric) hasUpDownBand() bool { return m.upThreshold != "" && m.downThreshold != "" }

// userSelectableAggregations are the aggregations an entry may name explicitly
// via the aggregation field. The pseudo-aggregations backing the readiness gate
// and the replica-count trigger are deliberately excluded: they report from the
// InferenceSet's status rather than a scrape
var userSelectableAggregations = map[string]struct{}{
	aggregator.SumAggregatorName:            {},
	aggregator.ServiceAverageAggregatorName: {},
	aggregator.ServiceSumAggregatorName:     {},
	constants.AggregationWindowedAvg:        {},
}

// aggregationForSpec resolves the aggregation for one entry, preferring an
// explicit override over the source- or type-derived default.
func aggregationForSpec(spec metricSpec, source string) (string, error) {
	var derived string
	switch spec.Type {
	case metricsTypeGauge:
		derived = aggregator.ServiceAverageAggregatorName
	case metricsTypeHistogram:
		derived = constants.AggregationWindowedAvg
	default:
		return "", fmt.Errorf("type must be %q or %q, got %q", metricsTypeGauge, metricsTypeHistogram, spec.Type)
	}
	// EPP gauges describe the router as a whole, not any one replica, so the
	// meaningful reduction across its pods is a sum rather than an average.
	if source == metricsource.EPPSourceName && spec.Type == metricsTypeGauge {
		derived = aggregator.ServiceSumAggregatorName
	}
	if spec.Aggregation == "" {
		return derived, nil
	}
	if _, ok := userSelectableAggregations[spec.Aggregation]; !ok {
		return "", fmt.Errorf("unsupported aggregation %q", spec.Aggregation)
	}
	// windowed-avg reads a histogram's _sum/_count pair out of the rolling
	// cache; pointing it at a gauge, or a gauge aggregation at a histogram,
	// would read fields the source never populates.
	if (spec.Aggregation == constants.AggregationWindowedAvg) != (spec.Type == metricsTypeHistogram) {
		return "", fmt.Errorf("aggregation %q is not compatible with type %q", spec.Aggregation, spec.Type)
	}
	return spec.Aggregation, nil
}

// sourceForMetricSource validates the metricsource annotation value, defaulting
// to "modelpod" when empty.
func sourceForMetricSource(s string) (string, error) {
	switch s {
	case "":
		return metricsource.ModelPodSourceName, nil
	case metricsource.ModelPodSourceName:
		return metricsource.ModelPodSourceName, nil
	case metricsource.EPPSourceName:
		return metricsource.EPPSourceName, nil
	default:
		return "", fmt.Errorf("source must be %q or %q, got %q", metricsource.ModelPodSourceName, metricsource.EPPSourceName, s)
	}
}

// metricsConfig is the fully parsed auto-provision configuration.
type metricsConfig struct {
	metrics           []metric
	policy            combinePolicy
	evaluationWindow  int32
	scaleUpCooldown   int32
	scaleDownCooldown int32
	cooldownPeriod    *int32
}

// ValidateConfig validates the auto-provision annotations, returning an
// error describing the first invalid field. It lets the controller admit or
// reject an InferenceSet's configuration without exposing the internal parsed
// representation.
//
// minReplicas selects the rule set: scale-to-zero configurations make the
// up/down band optional and add the activation rules, so the same annotations
// can be valid under one minimum and invalid under another.
func ValidateConfig(annotations map[string]string, minReplicas int) error {
	_, err := parseMetricsConfig(annotations, minReplicas)
	return err
}

// metricSpec is one entry of the scaledobject.kaito.sh/metrics annotation, a
// YAML (or JSON) list. Threshold and metriccachewindow fields are pointers so a
// missing key can be told apart from an explicit zero.
type metricSpec struct {
	// Name is the Prometheus metric name.
	Name string `json:"name"`
	// Type selects the aggregation: "gauge" or "histogram".
	Type string `json:"type"`
	// Source selects the metric source (optional, default "modelpod").
	Source string `json:"source,omitempty"`
	// UpThreshold is the scale-up threshold for the 1 -> N range. Required
	// unless the ScaledObject's minimum is 0, in which case it is optional but
	// must be paired with DownThreshold.
	UpThreshold *float64 `json:"upthreshold"`
	// DownThreshold is the scale-down threshold (must be <= upthreshold).
	DownThreshold *float64 `json:"downthreshold"`
	// ActivationThreshold wakes the workload (0 -> 1) when exceeded. Only valid
	// when the ScaledObject's minimum is 0, and only on a source observable
	// while the workload is parked.
	ActivationThreshold *float64 `json:"activationthreshold,omitempty"`
	// DeactivationThreshold parks the workload (1 -> 0) when every metric that
	// declares one falls below it. Only valid when the minimum is 0.
	DeactivationThreshold *float64 `json:"deactivationthreshold,omitempty"`
	// Aggregation overrides the aggregation otherwise derived from Type.
	Aggregation string `json:"aggregation,omitempty"`
	// MetricCacheWindow is the rolling cache window in seconds for histogram
	// metrics (optional, default 300). Only valid for histogram metrics.
	MetricCacheWindow *int `json:"metriccachewindow,omitempty"`
}

// parseMetricsConfig parses and validates the auto-provision annotations into a
// metricsConfig. The per-metric configuration is read from the
// scaledobject.kaito.sh/metrics annotation (a YAML list); the remaining global
// settings come from their own annotations. It returns an error describing the
// first invalid or missing field encountered.
//
// Only annotation-derived rules live here, so every caller can reach them --
// including the watch predicates, which have no API client and therefore cannot
// resolve the maximum. Rules that need the replica range are enforced
// separately by validateReplicaRange.
func parseMetricsConfig(annotations map[string]string, minReplicas int) (metricsConfig, error) {
	var cfg metricsConfig

	// Scale-to-zero is the only mode with a 0 <-> 1 range, so it is what makes
	// the activation thresholds meaningful and the up/down band optional.
	scaleToZero := minReplicas == 0

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

		// type is required and decides the aggregation unless overridden; source
		// is optional and selects the metric source (default "modelpod").
		source, err := sourceForMetricSource(spec.Source)
		if err != nil {
			return cfg, fmt.Errorf("metric %q (index %d): %w", spec.Name, i, err)
		}
		aggregation, err := aggregationForSpec(spec, source)
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

		// The readiness gate trigger is emitted on every ScaledObject, so a
		// metric sanitizing to its name has always been a live collision.
		if varName == gateTriggerName {
			return cfg, fmt.Errorf("metric %q (index %d) is reserved: it collides with the %q trigger", spec.Name, i, gateTriggerName)
		}
		// The replica-count trigger is only emitted under scale-to-zero, so the
		// name stays available to configurations that do not use it.
		if scaleToZero && varName == replicaCountTriggerName {
			return cfg, fmt.Errorf("metric %q (index %d) is reserved when min-replicas is 0: it collides with the %q trigger", spec.Name, i, replicaCountTriggerName)
		}

		// The up/down band is only optional under scale-to-zero.
		if !scaleToZero {
			if spec.UpThreshold == nil {
				return cfg, fmt.Errorf("metric %q (index %d): upthreshold is required", spec.Name, i)
			}
			if spec.DownThreshold == nil {
				return cfg, fmt.Errorf("metric %q (index %d): downthreshold is required", spec.Name, i)
			}
			if spec.ActivationThreshold != nil || spec.DeactivationThreshold != nil {
				return cfg, fmt.Errorf("metric %q (index %d): activationthreshold and deactivationthreshold require %s=\"0\"", spec.Name, i, constants.AnnotationKeyMinReplicas)
			}
		}

		// An unpaired bound describes half a band: the formula would grow without
		// ever shrinking, or the reverse.
		if (spec.UpThreshold == nil) != (spec.DownThreshold == nil) {
			return cfg, fmt.Errorf("metric %q (index %d): upthreshold and downthreshold must be declared together", spec.Name, i)
		}

		if spec.ActivationThreshold != nil && source != metricsource.EPPSourceName {
			return cfg, fmt.Errorf("metric %q (index %d): activationthreshold requires a source observable at zero replicas (%q), got %q", spec.Name, i, metricsource.EPPSourceName, source)
		}

		up, err := finiteThreshold(spec.UpThreshold, "upthreshold", spec.Name, i)
		if err != nil {
			return cfg, err
		}
		down, err := finiteThreshold(spec.DownThreshold, "downthreshold", spec.Name, i)
		if err != nil {
			return cfg, err
		}
		activation, err := finiteThreshold(spec.ActivationThreshold, "activationthreshold", spec.Name, i)
		if err != nil {
			return cfg, err
		}
		deactivation, err := finiteThreshold(spec.DeactivationThreshold, "deactivationthreshold", spec.Name, i)
		if err != nil {
			return cfg, err
		}
		if spec.UpThreshold != nil && spec.DownThreshold != nil && *spec.DownThreshold > *spec.UpThreshold {
			return cfg, fmt.Errorf("metric %q (index %d): downthreshold (%s) must not exceed upthreshold (%s)", spec.Name, i, down, up)
		}
		if spec.ActivationThreshold != nil && spec.DeactivationThreshold != nil && *spec.DeactivationThreshold > *spec.ActivationThreshold {
			return cfg, fmt.Errorf("metric %q (index %d): deactivationthreshold (%s) must not exceed activationthreshold (%s)", spec.Name, i, deactivation, activation)
		}
		if up == "" && down == "" && activation == "" && deactivation == "" {
			return cfg, fmt.Errorf("metric %q (index %d): at least one of upthreshold, downthreshold, activationthreshold, or deactivationthreshold is required", spec.Name, i)
		}

		// metriccachewindow is only meaningful for histogram metrics (windowed-avg
		// aggregation); default to defaultMetricCacheWindow and require a positive
		// number of seconds. It is rejected on gauge metrics.
		metricCacheWindow := ""
		if aggregation == constants.AggregationWindowedAvg {
			window := defaultMetricCacheWindow
			if spec.MetricCacheWindow != nil {
				window = *spec.MetricCacheWindow
				if window <= 0 {
					return cfg, fmt.Errorf("metric %q (index %d): metriccachewindow must be a positive number of seconds, got %d", spec.Name, i, window)
				}
			}
			metricCacheWindow = strconv.Itoa(window)
		} else if spec.MetricCacheWindow != nil {
			return cfg, fmt.Errorf("metric %q (index %d): metriccachewindow is only valid for histogram metrics", spec.Name, i)
		}

		cfg.metrics = append(cfg.metrics, metric{
			key:                   spec.Name,
			source:                source,
			aggregation:           aggregation,
			upThreshold:           up,
			downThreshold:         down,
			activationThreshold:   activation,
			deactivationThreshold: deactivation,
			metricCacheWindow:     metricCacheWindow,
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

	if scaleToZero {
		if err := validateActivationRules(cfg.metrics); err != nil {
			return cfg, err
		}
		cooldown, err := parseSecondsAnnotation(annotations, constants.AnnotationKeyCooldownPeriod, defaultCooldownPeriod)
		if err != nil {
			return cfg, err
		}
		cfg.cooldownPeriod = &cooldown
	} else if _, ok := annotations[constants.AnnotationKeyCooldownPeriod]; ok {
		return cfg, fmt.Errorf("%s requires %s=%q", constants.AnnotationKeyCooldownPeriod, constants.AnnotationKeyMinReplicas, "0")
	}

	return cfg, nil
}

// validateActivationRules enforces the cross-entry rules that only apply under
// scale-to-zero: the configuration must be able to both wake and park the
// workload.
func validateActivationRules(metrics []metric) error {
	var hasActivation, hasBackendDeactivation bool
	for _, m := range metrics {
		if m.activationThreshold != "" {
			hasActivation = true
		}
		if m.deactivationThreshold != "" && m.source == metricsource.ModelPodSourceName {
			hasBackendDeactivation = true
		}
	}
	if !hasActivation {
		return fmt.Errorf("%s=%q requires at least one metric with an activationthreshold, otherwise the workload can never wake",
			constants.AnnotationKeyMinReplicas, "0")
	}
	// A deactivation predicate built only from the router's own view could park
	// a replica that is still generating tokens. Requiring at least one
	// backend-observed signal captures that intent.
	if !hasBackendDeactivation {
		return fmt.Errorf("%s=%q requires at least one %q-sourced metric with a deactivationthreshold, so scale-down observes backend occupancy",
			constants.AnnotationKeyMinReplicas, "0", metricsource.ModelPodSourceName)
	}
	return nil
}

// validateReplicaRange enforces the one rule that needs the resolved maximum,
// so it cannot live in parseMetricsConfig: the watch predicates validate an
// InferenceSet without an API client and therefore cannot resolve a maximum
// derived from NodeCountLimit.
//
// It is scoped to scale-to-zero. A NodeCountLimit-derived min=1,max=1 range is
// provisioned with an up/down band on every metric, and rejecting it here
// would break a live configuration.
func validateReplicaRange(cfg metricsConfig, minReplicas, maxReplicas int) error {
	if minReplicas != 0 {
		return nil
	}
	var withBand int
	for _, m := range cfg.metrics {
		if m.hasUpDownBand() {
			withBand++
		}
	}
	if maxReplicas == 1 {
		if withBand > 0 {
			return fmt.Errorf("upthreshold/downthreshold are not allowed when %s is 1: there is no 1 to N range to scale over",
				constants.AnnotationKeyMaxReplicas)
		}
		return nil
	}
	if withBand == 0 {
		return fmt.Errorf("at least one metric must declare upthreshold and downthreshold when %s is greater than 1, otherwise the 1 to N range never scales",
			constants.AnnotationKeyMaxReplicas)
	}
	return nil
}

// finiteThreshold renders an optional threshold as the decimal string used in
// the formula and trigger metadata, returning "" when unset. YAML values like
// ".inf"/".nan" decode to non-finite floats; they are rejected here so they
// cannot break formula or trigger rendering downstream.
func finiteThreshold(v *float64, field, metricName string, index int) (string, error) {
	if v == nil {
		return "", nil
	}
	if math.IsInf(*v, 0) || math.IsNaN(*v) {
		return "", fmt.Errorf("metric %q (index %d): %s must be a finite number, got %s",
			metricName, index, field, strconv.FormatFloat(*v, 'f', -1, 64))
	}
	return strconv.FormatFloat(*v, 'f', -1, 64), nil
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

// buildFormula assembles the scalingModifiers expression for the given replica
// range, dispatching to the scale-to-zero shape only when the minimum is 0.
func buildFormula(cfg metricsConfig, minReplicas, maxReplicas int) string {
	if minReplicas == 0 {
		return buildScaleToZeroFormula(cfg, maxReplicas)
	}
	return buildAlwaysOnFormula(cfg)
}

// buildAlwaysOnFormula assembles the scalingModifiers expression used when the
// minimum is at least 1, so the workload never parks and the formula only has
// to cover the 1 <-> N range. It implements the conservative AND policy: scale
// up (multiplier 2.0) only when the readiness gate reports ready (== 1) AND
// every metric exceeds its up-threshold; scale down (multiplier 0.5) only when
// every metric is below its down-threshold; otherwise hold (1.0).
func buildAlwaysOnFormula(cfg metricsConfig) string {
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

// buildScaleToZeroFormula assembles the four-branch expression used when the
// minimum is 0. replica_count selects the range first, because the 0 -> 1
// decision is evaluated by KEDA's scaleFromZeroOrIdle path (which bypasses the
// HPA and only compares against activationTarget) while the 1 <-> N decision
// goes through the HPA ratio.
//
// The activation branch therefore emits 1.0 rather than scaleUpMultiplier: only
// "greater than the activation target" matters there, and the magnitude is
// discarded.
//
// Activation is an OR: any single metric crossing its threshold should wake the
// workload. Deactivation is an AND: every metric that declares a threshold must
// agree the workload is idle before it is parked.
func buildScaleToZeroFormula(cfg metricsConfig, maxReplicas int) string {
	var activationConds, deactivationConds, upConds, downConds []string
	for _, m := range cfg.metrics {
		v := formulaVarName(m.key)
		if m.activationThreshold != "" {
			activationConds = append(activationConds, fmt.Sprintf("%s > %s", v, m.activationThreshold))
		}
		if m.deactivationThreshold != "" {
			deactivationConds = append(deactivationConds, fmt.Sprintf("%s <= %s", v, m.deactivationThreshold))
		} else if m.activationThreshold != "" {
			// An activation signal must also be idle before a running workload can
			// park. This keeps queued work alive while its backend is provisioning.
			deactivationConds = append(deactivationConds, fmt.Sprintf("%s <= %s", v, m.activationThreshold))
		}
		if m.hasUpDownBand() {
			upConds = append(upConds, fmt.Sprintf("%s > %s", v, m.upThreshold))
			downConds = append(downConds, fmt.Sprintf("%s < %s", v, m.downThreshold))
		}
	}

	zeroBranch := fmt.Sprintf("(%s) ? %s : %s",
		joinPredicate(activationConds, "||"), activateMultiplier, deactivateMultiplier)

	// With no 1 <-> N range there is nothing between "parked" and "running",
	// so the sub-tree collapses to deactivate-or-hold.
	nonZeroBranch := fmt.Sprintf("(%s) ? %s : %s",
		joinPredicate(deactivationConds, "&&"), deactivateMultiplier, holdMultiplier)
	if maxReplicas > 1 {
		// The gate is prepended inside scaleUpExpr, so an empty condition set
		// would leave "readiness_gate == 1" as the whole predicate and grow the
		// workload on every ready evaluation. Collapse before the prepend.
		scaleUp := "false"
		if len(upConds) > 0 {
			scaleUp = cfg.policy.scaleUpExpr(gateTriggerName, upConds)
		}
		nonZeroBranch = fmt.Sprintf("(%s) ? %s : ((%s) ? %s : ((%s) ? %s : %s))",
			joinPredicate(deactivationConds, "&&"), deactivateMultiplier,
			scaleUp, scaleUpMultiplier,
			joinPredicate(downConds, "&&"), scaleDownMultiplier,
			holdMultiplier)
	}

	return fmt.Sprintf("(%s == 0) ? (%s) : (%s)", replicaCountTriggerName, zeroBranch, nonZeroBranch)
}

// joinPredicate combines conditions with the given operator, collapsing an
// empty set to the literal "false".
//
// The zero value matters: expr-lang treats an empty AND as vacuously true, so an
// empty deactivation or scale-down branch would fire unconditionally and park or
// shrink the workload on every evaluation. "false" makes an unpopulated branch
// inert instead, which is the safe reading of "the user configured nothing here".
func joinPredicate(conds []string, op string) string {
	if len(conds) == 0 {
		return "false"
	}
	return strings.Join(conds, " "+op+" ")
}

// buildTriggers builds one external trigger per metric (named after the
// sanitized metric name) plus the readiness gate trigger, and -- under
// scale-to-zero -- the replica-count trigger. All triggers use metricType Value;
// KEDA replaces their individual specs with a single composite spec derived from
// the scalingModifiers formula.
func (b Builder) buildTriggers(inferenceSetName, inferenceSetNamespace string, minReplicas int, cfg metricsConfig) []v1alpha1.ScaleTriggers {
	scaleToZero := minReplicas == 0
	scalerAddress := fmt.Sprintf("%s.%s.svc.cluster.local:%d", b.ScalerServiceName, b.ScalerNamespace, b.ScalerGRPCPort)
	authRef := &v1alpha1.AuthenticationRef{
		Name: constants.ClusterTriggerAuthName,
		Kind: constants.ClusterTriggerAuthKind,
	}

	triggers := make([]v1alpha1.ScaleTriggers, 0, len(cfg.metrics)+2)
	for _, m := range cfg.metrics {
		metadata := map[string]string{
			constants.InferenceSetNameInMetadata:      inferenceSetName,
			constants.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
			constants.ScalerAddressInMetadata:         scalerAddress,
			constants.MetricNameInMetadata:            m.key,
			constants.MetricSourceInMetadata:          m.source,
			constants.AggregationInMetadata:           m.aggregation,
		}
		if m.metricCacheWindow != "" {
			metadata[constants.MetricCacheWindowInMetadata] = m.metricCacheWindow
		}
		// The scaler's scrape defaults describe the workspace Service (port 80).
		// The EPP exposes its own endpoint, so it has to be stated explicitly.
		if m.source == metricsource.EPPSourceName {
			metadata[constants.MetricPortInMetadata] = eppMetricsPort
			metadata[constants.MetricPathInMetadata] = eppMetricsPath
		}
		// Opt-in rather than inferred by the scaler: a modelpod metric has
		// nothing to scrape at zero replicas, but only a ScaledObject that can
		// actually reach zero should read that as 0 instead of an error.
		if scaleToZero && m.source == metricsource.ModelPodSourceName {
			metadata[constants.ZeroReplicaFallbackInMetadata] = "true"
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

	// Replica-count trigger: emitted only under scale-to-zero, where the formula
	// needs it to pick between the 0 -> 1 and 1 <-> N ranges. Omitting it
	// otherwise keeps the metric name available to existing configurations.
	if scaleToZero {
		triggers = append(triggers, v1alpha1.ScaleTriggers{
			Type: "external",
			Name: replicaCountTriggerName,
			Metadata: map[string]string{
				constants.InferenceSetNameInMetadata:      inferenceSetName,
				constants.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
				constants.ScalerAddressInMetadata:         scalerAddress,
				constants.MetricNameInMetadata:            replicaCountTriggerName,
				constants.AggregationInMetadata:           constants.AggregationReplicas,
			},
			AuthenticationRef: authRef,
			MetricType:        autoscalingv2.ValueMetricType,
		})
	}

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
	scaleToZero := minReplicas == 0

	modifiers := v1alpha1.ScalingModifiers{
		Formula:    buildFormula(cfg, minReplicas, maxReplicas),
		Target:     scalingTarget,
		MetricType: autoscalingv2.ValueMetricType,
	}
	if scaleToZero {
		// Stated explicitly rather than relying on KEDA's implicit default, so
		// the 0 <-> 1 contract is visible in the rendered object: the formula's
		// activate/deactivate multipliers only mean anything relative to it.
		modifiers.ActivationTarget = activationTarget
	}

	so := &v1alpha1.ScaledObject{
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
				ScalingModifiers:              modifiers,
			},
			ScaleTargetRef: &v1alpha1.ScaleTarget{
				Name:       is.Name,
				APIVersion: constants.InferenceSetAPIVersion,
				Kind:       constants.InferenceSet,
			},
			PollingInterval: ptr.To(int32(defaultPollingInterval)),
			MinReplicaCount: ptr.To(int32(minReplicas)),
			MaxReplicaCount: ptr.To(int32(maxReplicas)),
			Triggers:        b.buildTriggers(is.Name, is.Namespace, minReplicas, cfg),
		},
	}
	if scaleToZero {
		so.Spec.CooldownPeriod = cfg.cooldownPeriod
	}
	return so
}
