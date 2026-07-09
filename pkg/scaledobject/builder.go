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
	"math"
	"strconv"
	"strings"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
)

const (
	AnnotationKeyThreshold  = "scaledobject.kaito.sh/threshold"
	AnnotationKeyMetricName = "scaledobject.kaito.sh/metricName"

	AnnotationKeyManagedBy = "scaledobject.kaito.sh/managed-by"
	InferenceSet           = "InferenceSet"

	inferenceSetAPIVersion = "kaito.sh/v1beta1"

	// defaultMetricName is the metric scraped from each vLLM inference pod when
	// the user does not override it via the metricName annotation.
	defaultMetricName = "vllm:num_requests_waiting"

	// defaultPollingInterval is the interval (in seconds) at which KEDA polls
	// the external scaler for metrics. Overrides KEDA's built-in default of 30s
	// to give the autoscaler fresher signals for latency-sensitive inference
	// workloads.
	defaultPollingInterval = 15

	// Composite (multi-metric) autoscaling annotations. Presence of the indexed
	// metricName/0 annotation switches the InferenceSet into composite mode,
	// where multiple signals are combined via a conservative AND policy using a
	// KEDA scalingModifiers formula.
	//
	// Indexed keys use the base key with a "/{i}" suffix:
	//   scaledobject.kaito.sh/metricName/0, .../metricName/1, ...
	//   scaledobject.kaito.sh/upthreshold/0, .../upthreshold/1, ...
	//   scaledobject.kaito.sh/downthreshold/0, .../downthreshold/1, ...
	AnnotationKeyCombinePolicy     = "scaledobject.kaito.sh/combinepolicy"
	AnnotationKeyEvaluationWindow  = "scaledobject.kaito.sh/evaluationwindow"
	AnnotationKeyScaleUpCooldown   = "scaledobject.kaito.sh/scaleupcooldown"
	AnnotationKeyScaleDownCooldown = "scaledobject.kaito.sh/scaledowncooldown"

	// Base keys for the indexed per-metric threshold and quantile annotations.
	annotationKeyUpThreshold   = "scaledobject.kaito.sh/upthreshold"
	annotationKeyDownThreshold = "scaledobject.kaito.sh/downthreshold"
	annotationKeyQuantile      = "scaledobject.kaito.sh/quantile"

	// combinePolicyAnd is the only supported combine policy: scale up only when
	// every metric is above its up-threshold, scale down only when every metric
	// is below its down-threshold.
	combinePolicyAnd = "AND"

	// Composite scaling formula multipliers. The formula is evaluated relative to
	// the current replica count (target = 1, metricType = Value), so the output
	// is the desired-to-current ratio: >1 scales up, <1 scales down, 1 holds.
	// The HPA Pods policy (value 1) caps the actual change to +/-1 replica per
	// cooldown, giving a conservative single-step behaviour.
	compositeScaleUpMultiplier   = "2.0"
	compositeScaleDownMultiplier = "0.5"
	compositeHoldMultiplier      = "1.0"

	// compositeGateTriggerName is the reserved trigger/variable name for the
	// readiness gate in the scalingModifiers formula. Metric triggers instead use
	// their (sanitized) metric name as the variable name, so the formula reads in
	// terms of the real metrics rather than opaque indexed placeholders.
	compositeGateTriggerName = "readiness_gate"

	// defaultCompositeQuantile is the target quantile applied to quantile-based
	// composite metrics when the per-metric quantile annotation is absent (p95).
	defaultCompositeQuantile = "0.95"

	// Composite defaults (seconds) when the corresponding annotation is absent.
	defaultEvaluationWindow  = 60
	defaultScaleUpCooldown   = 300
	defaultScaleDownCooldown = 300

	// compositeTolerance must be strictly below the scale-down multiplier's
	// distance from 1 (|0.5 - 1| = 0.5) so a 0.5 ratio still triggers a
	// scale-down, while a hold ratio of 1.0 is left untouched.
	compositeTolerance = "0.1"
)

// combinePolicy expresses how per-metric conditions are combined into the
// scale-up and scale-down predicates of the composite scalingModifiers formula.
// Abstracting it behind an interface keeps buildCompositeFormula agnostic of the
// concrete policy and leaves room for future policies (e.g. OR) without touching
// the formula assembly.
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

// andCombinePolicy is the conservative policy: scale up only when the gate is
// clear AND every metric is above its up-threshold; scale down only when every
// metric is below its down-threshold.
type andCombinePolicy struct{}

func (andCombinePolicy) name() string { return combinePolicyAnd }

func (andCombinePolicy) scaleUpExpr(gateVar string, upConds []string) string {
	conds := append([]string{fmt.Sprintf("%s == 0", gateVar)}, upConds...)
	return strings.Join(conds, " && ")
}

func (andCombinePolicy) scaleDownExpr(downConds []string) string {
	return strings.Join(downConds, " && ")
}

// combinePolicies is the registry of supported combine policies keyed by name.
var combinePolicies = map[string]combinePolicy{
	combinePolicyAnd: andCombinePolicy{},
}

// metricCatalogEntry maps a composite metric name to the concrete scraper source
// and aggregation the scaler must use to serve it.
type metricCatalogEntry struct {
	source      string
	aggregation string
}

// compositeMetricCatalog is the fixed set of composite metrics the controller
// understands. The map key is the metric name users reference via the
// metricName/{i} annotations, so no extra naming indirection is introduced.
var compositeMetricCatalog = map[string]metricCatalogEntry{
	// EPP-derived per-pod queue length, averaged with float precision.
	"inference_pool_per_pod_queue_size": {
		source:      "epp",
		aggregation: "perpod-avg",
	},
	// EPP-derived end-to-end request latency via configurable histogram quantile.
	"inference_objective_request_duration_seconds": {
		source:      "epp",
		aggregation: "quantile",
	},
	// vLLM waiting-queue length scraped directly from workspace Services, summed.
	"vllm:num_requests_waiting": {
		source:      "service",
		aggregation: "sum",
	},
}

// compositeMetric is one parsed entry of a composite configuration.
type compositeMetric struct {
	key           string
	entry         metricCatalogEntry
	upThreshold   string
	downThreshold string
	// quantile is the target quantile (as a string, e.g. "0.95") for metrics
	// using the "quantile" aggregation; empty for other aggregations.
	quantile string
}

// compositeConfig is the fully parsed multi-metric autoscaling configuration.
type compositeConfig struct {
	metrics           []compositeMetric
	policy            combinePolicy
	evaluationWindow  int32
	scaleUpCooldown   int32
	scaleDownCooldown int32
}

// IsCompositeMode reports whether the InferenceSet opts into composite
// (multi-metric) autoscaling, signalled by the presence of the first indexed
// metric annotation (metricName/0).
func IsCompositeMode(annotations map[string]string) bool {
	return annotations[compositeIndexedKey(AnnotationKeyMetricName, 0)] != ""
}

// ValidateComposite validates the composite autoscaling annotations, returning
// an error describing the first invalid field. It lets the controller admit or
// reject an InferenceSet's composite configuration without exposing the internal
// parsed representation.
func ValidateComposite(annotations map[string]string) error {
	_, err := parseCompositeConfig(annotations)
	return err
}

// resolveMetricName returns the metric name configured via the metricName
// annotation on the InferenceSet, falling back to defaultMetricName when
// the annotation is absent or empty.
func resolveMetricName(annotations map[string]string) string {
	if name, ok := annotations[AnnotationKeyMetricName]; ok && name != "" {
		return name
	}
	return defaultMetricName
}

// buildScaledObject builds the single-metric ScaledObject: a single scraped
// metric compared against a threshold.
func (b Builder) buildScaledObject(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int, threshold, metricName string) *v1alpha1.ScaledObject {
	return &v1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      is.Name,
			Namespace: is.Namespace,
			Annotations: map[string]string{
				AnnotationKeyManagedBy: scaler.ScalerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         inferenceSetAPIVersion,
					Kind:               InferenceSet,
					Name:               is.Name,
					UID:                is.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.ScaledObjectSpec{
			Advanced: &v1alpha1.AdvancedConfig{
				HorizontalPodAutoscalerConfig: getDefaultHorizontalPodAutoscalerConfig(),
			},
			ScaleTargetRef: &v1alpha1.ScaleTarget{
				Name:       is.Name,
				APIVersion: inferenceSetAPIVersion,
				Kind:       InferenceSet,
			},
			PollingInterval: ptr.To(int32(defaultPollingInterval)),
			MinReplicaCount: ptr.To(int32(minReplicas)),
			MaxReplicaCount: ptr.To(int32(maxReplicas)),
			Triggers:        getDefaultKedaKaitoScalerTriggers(is.Name, is.Namespace, b.ScalerNamespace, b.ScalerServiceName, b.ScalerGRPCPort, threshold, metricName),
		},
	}
}

// compositeIndexedKey builds an indexed annotation key such as
// "scaledobject.kaito.sh/metricName/0".
func compositeIndexedKey(base string, i int) string {
	return fmt.Sprintf("%s/%d", base, i)
}

// parseCompositeConfig parses and validates the composite autoscaling
// annotations into a compositeConfig. It returns an error describing the first
// invalid or missing field encountered.
func parseCompositeConfig(annotations map[string]string) (compositeConfig, error) {
	var cfg compositeConfig

	// Resolve the combine policy (default AND). Only registered policies are
	// accepted.
	policyName := combinePolicyAnd
	if p, ok := annotations[AnnotationKeyCombinePolicy]; ok && p != "" {
		policyName = strings.ToUpper(p)
	}
	policy, ok := combinePolicies[policyName]
	if !ok {
		return cfg, fmt.Errorf("unsupported combinepolicy %q", annotations[AnnotationKeyCombinePolicy])
	}
	cfg.policy = policy

	seenVars := make(map[string]string)
	for i := 0; ; i++ {
		key := annotations[compositeIndexedKey(AnnotationKeyMetricName, i)]
		if key == "" {
			break
		}
		entry, ok := compositeMetricCatalog[key]
		if !ok {
			return cfg, fmt.Errorf("unknown composite metric %q at index %d", key, i)
		}

		// The metric name doubles as the formula variable/trigger name after
		// sanitization; two metrics collapsing to the same variable would make the
		// formula ambiguous and KEDA reject duplicate trigger names.
		varName := formulaVarName(key)
		if prev, dup := seenVars[varName]; dup {
			return cfg, fmt.Errorf("metric %q (index %d) collides with %q on formula variable %q", key, i, prev, varName)
		}
		seenVars[varName] = key

		upStr := annotations[compositeIndexedKey(annotationKeyUpThreshold, i)]
		downStr := annotations[compositeIndexedKey(annotationKeyDownThreshold, i)]
		up, err := strconv.ParseFloat(upStr, 64)
		if err != nil {
			return cfg, fmt.Errorf("metric %q (index %d): upthreshold must be a valid number, got %q", key, i, upStr)
		}
		down, err := strconv.ParseFloat(downStr, 64)
		if err != nil {
			return cfg, fmt.Errorf("metric %q (index %d): downthreshold must be a valid number, got %q", key, i, downStr)
		}
		if down > up {
			return cfg, fmt.Errorf("metric %q (index %d): downthreshold (%s) must not exceed upthreshold (%s)", key, i, downStr, upStr)
		}

		// Quantile is only meaningful for quantile-aggregated metrics; default to
		// p95 and validate the (0, 1] range when provided.
		quantile := ""
		if entry.aggregation == "quantile" {
			quantile = defaultCompositeQuantile
			if q := annotations[compositeIndexedKey(annotationKeyQuantile, i)]; q != "" {
				qv, err := strconv.ParseFloat(q, 64)
				if err != nil {
					return cfg, fmt.Errorf("metric %q (index %d): quantile must be a valid number, got %q", key, i, q)
				}
				if qv <= 0 || qv > 1 {
					return cfg, fmt.Errorf("metric %q (index %d): quantile must be in the (0, 1] range, got %q", key, i, q)
				}
				quantile = strconv.FormatFloat(qv, 'f', -1, 64)
			}
		}

		cfg.metrics = append(cfg.metrics, compositeMetric{
			key:           key,
			entry:         entry,
			upThreshold:   strconv.FormatFloat(up, 'f', -1, 64),
			downThreshold: strconv.FormatFloat(down, 'f', -1, 64),
			quantile:      quantile,
		})
	}

	if len(cfg.metrics) == 0 {
		return cfg, fmt.Errorf("composite mode requires at least one metricName/0 annotation")
	}

	cfg.evaluationWindow = parseSecondsAnnotation(annotations, AnnotationKeyEvaluationWindow, defaultEvaluationWindow)
	cfg.scaleUpCooldown = parseSecondsAnnotation(annotations, AnnotationKeyScaleUpCooldown, defaultScaleUpCooldown)
	cfg.scaleDownCooldown = parseSecondsAnnotation(annotations, AnnotationKeyScaleDownCooldown, defaultScaleDownCooldown)

	return cfg, nil
}

func parseSecondsAnnotation(annotations map[string]string, key string, def int32) int32 {
	if v, ok := annotations[key]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= math.MaxInt32 {
			return int32(n)
		}
	}
	return def
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

// buildCompositeFormula assembles the scalingModifiers expression implementing
// the conservative AND policy: scale up (multiplier 2.0) only when the readiness
// gate is clear AND every metric exceeds its up-threshold; scale down
// (multiplier 0.5) only when every metric is below its down-threshold; otherwise
// hold (1.0).
func buildCompositeFormula(cfg compositeConfig) string {
	upConds := make([]string, 0, len(cfg.metrics))
	downConds := make([]string, 0, len(cfg.metrics))
	for _, m := range cfg.metrics {
		v := formulaVarName(m.key)
		upConds = append(upConds, fmt.Sprintf("%s > %s", v, m.upThreshold))
		downConds = append(downConds, fmt.Sprintf("%s < %s", v, m.downThreshold))
	}
	return fmt.Sprintf("(%s) ? %s : ((%s) ? %s : %s)",
		cfg.policy.scaleUpExpr(compositeGateTriggerName, upConds), compositeScaleUpMultiplier,
		cfg.policy.scaleDownExpr(downConds), compositeScaleDownMultiplier,
		compositeHoldMultiplier)
}

// buildCompositeTriggers builds one external trigger per composite metric (named
// after the sanitized metric name) plus the readiness gate trigger. All triggers
// use metricType Value; KEDA replaces their individual specs with a single
// composite spec derived from the scalingModifiers formula.
func (b Builder) buildCompositeTriggers(inferenceSetName, inferenceSetNamespace string, cfg compositeConfig) []v1alpha1.ScaleTriggers {
	scalerAddress := fmt.Sprintf("%s.%s.svc.cluster.local:%d", b.ScalerServiceName, b.ScalerNamespace, b.ScalerGRPCPort)
	authRef := &v1alpha1.AuthenticationRef{
		Name: scaler.ClusterTriggerAuthName,
		Kind: scaler.ClusterTriggerAuthKind,
	}

	triggers := make([]v1alpha1.ScaleTriggers, 0, len(cfg.metrics)+1)
	for _, m := range cfg.metrics {
		metadata := map[string]string{
			// threshold is required by the scaler metadata parser but ignored
			// in composite mode (KEDA overrides per-trigger targets with the
			// composite spec).
			"threshold":                            "1",
			scaler.InferenceSetNameInMetadata:      inferenceSetName,
			scaler.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
			scaler.ScalerAddressInMetadata:         scalerAddress,
			scaler.MetricNameInMetadata:            m.key,
			scaler.MetricSourceInMetadata:          m.entry.source,
			scaler.AggregationInMetadata:           m.entry.aggregation,
		}
		if m.quantile != "" {
			metadata[scaler.QuantileInMetadata] = m.quantile
		}
		triggers = append(triggers, v1alpha1.ScaleTriggers{
			Type:              "external",
			Name:              formulaVarName(m.key),
			Metadata:          metadata,
			AuthenticationRef: authRef,
			MetricType:        autoscalingv2.ValueMetricType,
		})
	}

	// Readiness gate trigger: reports 1 while replicas are still becoming ready.
	triggers = append(triggers, v1alpha1.ScaleTriggers{
		Type: "external",
		Name: compositeGateTriggerName,
		Metadata: map[string]string{
			"threshold":                            "1",
			scaler.InferenceSetNameInMetadata:      inferenceSetName,
			scaler.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
			scaler.ScalerAddressInMetadata:         scalerAddress,
			scaler.MetricNameInMetadata:            compositeGateTriggerName,
			scaler.AggregationInMetadata:           scaler.AggregationGate,
		},
		AuthenticationRef: authRef,
		MetricType:        autoscalingv2.ValueMetricType,
	})

	return triggers
}

// getCompositeHorizontalPodAutoscalerConfig builds the HPA behaviour for
// composite mode: the evaluation window gates scale-up stabilization, the
// cooldowns bound how often replicas may change, and the tolerance is tightened
// to 0.1 in both directions so the 0.5 scale-down ratio is not swallowed.
func getCompositeHorizontalPodAutoscalerConfig(cfg compositeConfig) *v1alpha1.HorizontalPodAutoscalerConfig {
	tolerance := func() *resource.Quantity {
		q := resource.MustParse(compositeTolerance)
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

func (b Builder) buildCompositeScaledObject(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int, cfg compositeConfig) *v1alpha1.ScaledObject {
	return &v1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      is.Name,
			Namespace: is.Namespace,
			Annotations: map[string]string{
				AnnotationKeyManagedBy: scaler.ScalerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         inferenceSetAPIVersion,
					Kind:               InferenceSet,
					Name:               is.Name,
					UID:                is.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: v1alpha1.ScaledObjectSpec{
			Advanced: &v1alpha1.AdvancedConfig{
				HorizontalPodAutoscalerConfig: getCompositeHorizontalPodAutoscalerConfig(cfg),
				ScalingModifiers: v1alpha1.ScalingModifiers{
					Formula: buildCompositeFormula(cfg),
					// target = 1 makes the formula output the desired-to-current
					// replica ratio; metricType Value applies it multiplicatively.
					Target:     "1",
					MetricType: autoscalingv2.ValueMetricType,
				},
			},
			ScaleTargetRef: &v1alpha1.ScaleTarget{
				Name:       is.Name,
				APIVersion: inferenceSetAPIVersion,
				Kind:       InferenceSet,
			},
			PollingInterval: ptr.To(int32(defaultPollingInterval)),
			MinReplicaCount: ptr.To(int32(minReplicas)),
			MaxReplicaCount: ptr.To(int32(maxReplicas)),
			Triggers:        b.buildCompositeTriggers(is.Name, is.Namespace, cfg),
		},
	}
}

func getDefaultHorizontalPodAutoscalerConfig() *v1alpha1.HorizontalPodAutoscalerConfig {
	return &v1alpha1.HorizontalPodAutoscalerConfig{
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(60)),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: 300,
					},
				},
				Tolerance: func() *resource.Quantity {
					q := resource.MustParse("0.1")
					return &q
				}(),
			},
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To(int32(300)),
				SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
				Policies: []autoscalingv2.HPAScalingPolicy{
					{
						Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
						Value:         1,
						PeriodSeconds: 300,
					},
				},
				Tolerance: func() *resource.Quantity {
					q := resource.MustParse("0.5")
					return &q
				}(),
			},
		},
	}
}

func getDefaultKedaKaitoScalerTriggers(inferenceSetName, inferenceSetNamespace, scalerNamespace, scalerServiceName string, scalerGRPCPort int, threshold, metricName string) []v1alpha1.ScaleTriggers {
	// metricProtocol/metricPort/metricPath/scrapeTimeout are intentionally
	// omitted: the scaler-side parseScalerMetadata fills them with defaults
	// matching Kaito's vLLM exposure (http on Service port 80, /metrics, 3s),
	// so we only keep keys that the scaler cannot infer on its own.
	return []v1alpha1.ScaleTriggers{
		{
			Type: "external",
			Name: scaler.ScalerName,
			Metadata: map[string]string{
				"threshold":                            threshold,
				scaler.InferenceSetNameInMetadata:      inferenceSetName,
				scaler.InferenceSetNamespaceInMetadata: inferenceSetNamespace,
				scaler.ScalerAddressInMetadata:         fmt.Sprintf("%s.%s.svc.cluster.local:%d", scalerServiceName, scalerNamespace, scalerGRPCPort),
				scaler.MetricNameInMetadata:            metricName,
			},
			AuthenticationRef: &v1alpha1.AuthenticationRef{
				Name: scaler.ClusterTriggerAuthName,
				Kind: scaler.ClusterTriggerAuthKind,
			},
			MetricType: autoscalingv2.AverageValueMetricType,
		},
	}
}
