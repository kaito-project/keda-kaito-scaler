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

// Package constants defines the InferenceSet autoscaling annotation keys and
// related identifiers shared across the scaler packages.
package constants

const (
	// AnnotationKeyManagedBy marks a ScaledObject as managed by keda-kaito-scaler.
	AnnotationKeyManagedBy = "scaledobject.kaito.sh/managed-by"
	// AnnotationKeyAutoProvision enables auto-provisioning when set to "true".
	AnnotationKeyAutoProvision = "scaledobject.kaito.sh/auto-provision"
	// AnnotationKeyMinReplicas sets the minimum replica count.
	AnnotationKeyMinReplicas = "scaledobject.kaito.sh/min-replicas"
	// AnnotationKeyMaxReplicas sets the maximum replica count.
	AnnotationKeyMaxReplicas = "scaledobject.kaito.sh/max-replicas"

	// --- Auto-provision annotations ---

	// AnnotationKeyMetrics carries the auto-provision metrics as a YAML list.
	// Each entry configures one metric with the fields: name, type,
	// source (optional), upthreshold, downthreshold, and metriccachewindow
	// (optional, histogram only).
	// When auto-provision="true", this annotation is required and must contain
	// at least one metric; otherwise auto-provisioning fails with an error.
	AnnotationKeyMetrics = "scaledobject.kaito.sh/metrics"

	// Global keys.

	// AnnotationKeyCombinePolicy selects how metrics are combined (only AND).
	AnnotationKeyCombinePolicy = "scaledobject.kaito.sh/combinepolicy"
	// AnnotationKeyEvaluationWindow sets the scale-up stabilization window (seconds).
	AnnotationKeyEvaluationWindow = "scaledobject.kaito.sh/evaluationwindow"
	// AnnotationKeyScaleUpCooldown sets the minimum seconds between scale-up steps.
	AnnotationKeyScaleUpCooldown = "scaledobject.kaito.sh/scaleupcooldown"
	// AnnotationKeyScaleDownCooldown sets the minimum seconds between scale-down steps.
	AnnotationKeyScaleDownCooldown = "scaledobject.kaito.sh/scaledowncooldown"

	// InferenceSet is the Kind of the scale target referenced by managed ScaledObjects.
	InferenceSet = "InferenceSet"
	// InferenceSetAPIVersion is the apiVersion of the InferenceSet scale target.
	InferenceSetAPIVersion = "kaito.sh/v1beta1"

	// ClusterTriggerAuthName is the cluster-scoped ClusterTriggerAuthentication
	// carrying the scaler's mTLS credentials.
	ClusterTriggerAuthName = "keda-kaito-scaler-creds"
	// ClusterTriggerAuthKind is the KEDA authenticationRef kind.
	ClusterTriggerAuthKind = "ClusterTriggerAuthentication"

	// --- Scaler trigger metadata keys ---

	// InferenceSetNameInMetadata identifies the target InferenceSet name.
	InferenceSetNameInMetadata = "inferenceSetName"
	// InferenceSetNamespaceInMetadata identifies the target InferenceSet namespace.
	InferenceSetNamespaceInMetadata = "inferenceSetNamespace"
	// ScalerAddressInMetadata is the gRPC address of the external scaler.
	ScalerAddressInMetadata = "scalerAddress"
	// MetricNameInMetadata is the metric name served by the trigger.
	MetricNameInMetadata = "metricName"
	// ThresholdInMetadata is the per-replica scale threshold.
	ThresholdInMetadata = "threshold"
	// MetricProtocolInMetadata is the scrape protocol ("http" or "https").
	MetricProtocolInMetadata = "metricProtocol"
	// MetricPortInMetadata is the scrape port.
	MetricPortInMetadata = "metricPort"
	// MetricPathInMetadata is the scrape path.
	MetricPathInMetadata = "metricPath"
	// ScrapeTimeoutInMetadata bounds a single scrape.
	ScrapeTimeoutInMetadata = "scrapeTimeout"
	// MetricSourceInMetadata selects which registered metric source is used.
	MetricSourceInMetadata = "metricSource"
	// AggregationInMetadata selects which registered aggregator is used.
	AggregationInMetadata = "aggregation"
	// MetricCacheWindowInMetadata sets the rolling window (bare seconds, e.g.
	// "300") over which the windowed-avg aggregation averages a histogram
	// metric's _sum/_count. Only carried by histogram triggers.
	MetricCacheWindowInMetadata = "metricCacheWindow"

	// AggregationWindowedAvg averages a histogram metric's observations over a
	// rolling cache window (metricCacheWindow) using _sum/_count deltas instead
	// of bucket interpolation. The scaler serves it from an in-memory cache that
	// a background poller keeps fresh, so it needs no per-request scrape.
	AggregationWindowedAvg = "windowed-avg"

	// AggregationGate is a pseudo-aggregation: instead of scraping, the scaler
	// reports 1 when every desired replica of the InferenceSet is ready
	// (readyReplicas >= spec replicas) and 0 otherwise. It backs the readiness
	// "gate" used by composite scaling formulas to hold scale decisions until pods
	// are ready.
	AggregationGate = "gate"
)
