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

package scaler

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scraper"
)

const (
	ScalerName = "keda-kaito-scaler"

	// ClusterTriggerAuthName is the name of the cluster-scoped
	// ClusterTriggerAuthentication created/managed by keda-kaito-scaler so
	// KEDA can load the mTLS credentials of the external scaler. It is
	// referenced by both the bootstrap code that creates the resource and
	// by the autoprovision controller when wiring up ScaledObject triggers.
	ClusterTriggerAuthName = "keda-kaito-scaler-creds"

	// ClusterTriggerAuthKind is the KEDA kind name used in
	// ScaledObject.spec.triggers[*].authenticationRef.kind.
	ClusterTriggerAuthKind = "ClusterTriggerAuthentication"

	// Metadata keys
	InferenceSetNameInMetadata      = "inferenceSetName"
	InferenceSetNamespaceInMetadata = "inferenceSetNamespace"
	ScalerAddressInMetadata         = "scalerAddress"
	MetricNameInMetadata            = "metricName"
	ThresholdInMetadata             = "threshold"
	MetricProtocolInMetadata        = "metricProtocol"
	MetricPortInMetadata            = "metricPort"
	MetricPathInMetadata            = "metricPath"
	ScrapeTimeoutInMetadata         = "scrapeTimeout"
	// MetricSourceInMetadata selects which registered scraper produces the
	// metrics snapshot (e.g. "service" for workspace vLLM Services).
	// AggregationInMetadata selects which registered aggregator reduces that
	// snapshot to a single value (e.g. "sum", "service-avg", "quantile"), or the
	// special "gate" aggregation which needs no scrape.
	MetricSourceInMetadata = "metricSource"
	AggregationInMetadata  = "aggregation"
	// QuantileInMetadata sets the target quantile in (0, 1] for the "quantile"
	// aggregation (e.g. "0.95" for p95). Ignored by other aggregations.
	QuantileInMetadata = "quantile"

	// Defaults applied when the corresponding metadata key is omitted. Only
	// inferenceSetName/inferenceSetNamespace/metricName/threshold remain
	// mandatory; everything else falls back to a sensible value matching
	// Kaito's current vLLM exposure conventions.
	defaultMetricProtocol = "http"
	defaultMetricPort     = "80"
	defaultMetricPath     = "/metrics"
	defaultScrapeTimeout  = 3 * time.Second

	// defaultMetricSource / defaultAggregation preserve the original
	// single-metric behaviour (scrape workspace Services, sum across them) when
	// the new metadata keys are absent.
	defaultMetricSource = "service"
	defaultAggregation  = "sum"
	// defaultQuantile is the target quantile used by the "quantile" aggregation
	// when the quantile metadata key is omitted (p95).
	defaultQuantile = 0.95

	// AggregationGate is a pseudo-aggregation: instead of scraping, the scaler
	// reports 1 when the InferenceSet has not-yet-ready replicas (readyReplicas <
	// spec replicas) and 0 otherwise. It backs the readiness "gate" used by
	// composite scaling formulas to hold scale decisions until pods are ready.
	AggregationGate = "gate"

	// AggregationQuantile computes a configurable histogram quantile. It is
	// parametric (see QuantileInMetadata), so the scaler builds the aggregator
	// per request instead of looking it up in the static aggregator map.
	AggregationQuantile = "quantile"
)

// Config is the parsed scaler metadata payload sent by KEDA for every request.
type Config struct {
	InferenceSetName      string
	InferenceSetNamespace string
	MetricName            string
	MetricProtocol        string
	MetricPort            string
	MetricPath            string
	ScrapeTimeout         time.Duration
	Threshold             float64
	// MetricSource / Aggregation select the scraper and aggregator used to serve
	// this trigger. They default to "service"/"sum" for backward compatibility.
	MetricSource string
	Aggregation  string
	// Quantile is the target quantile in (0, 1] for the "quantile" aggregation.
	Quantile float64
}

// scrapeConfig projects the subset of Config needed by the metrics Scraper.
func (c *Config) scrapeConfig() scraper.ScrapeConfig {
	return scraper.ScrapeConfig{
		Protocol: c.MetricProtocol,
		Port:     c.MetricPort,
		Path:     c.MetricPath,
		Timeout:  c.ScrapeTimeout,
	}
}

// KaitoScaler implements the KEDA external scaler gRPC contract on top of
// pluggable scraper.Scrapers (which fetch raw per-service metrics) and
// aggregator.Aggregators (which reduce them to the single value KEDA expects).
// A trigger selects its scraper/aggregator via the metricSource/aggregation
// metadata keys, letting one scaler serve both the legacy single-metric path
// and the new composite (queue length + latency + readiness gate) triggers.
type KaitoScaler struct {
	kubeClient  client.Client
	scrapers    map[string]scraper.Scraper
	aggregators map[string]aggregator.Aggregator
	// quantileAggregators caches the parametric quantile aggregators keyed by
	// their target quantile (float64 -> aggregator.Aggregator). QuantileAggregator
	// is immutable and safe for concurrent use, so one instance can be shared by
	// every request targeting the same quantile instead of allocating a new one
	// on each GetMetrics call.
	quantileAggregators sync.Map
	externalscaler.UnimplementedExternalScalerServer
}

// NewKaitoScaler wires the Kubernetes client, the set of named metrics scrapers
// and the set of named aggregators used to serve KEDA scaling requests. The
// maps must at least contain the "service" scraper and the "sum" aggregator so
// the default single-metric path keeps working.
func NewKaitoScaler(kubeClient client.Client, scrapers map[string]scraper.Scraper, aggregators map[string]aggregator.Aggregator) *KaitoScaler {
	return &KaitoScaler{
		kubeClient:  kubeClient,
		scrapers:    scrapers,
		aggregators: aggregators,
	}
}

// quantileAggregator returns the QuantileAggregator for the given quantile,
// creating and caching it on first use. The aggregator is immutable and safe
// for concurrent use, so subsequent requests with the same quantile reuse the
// cached instance instead of allocating a new one.
func (e *KaitoScaler) quantileAggregator(quantile float64) aggregator.Aggregator {
	if agg, ok := e.quantileAggregators.Load(quantile); ok {
		return agg.(aggregator.Aggregator)
	}
	agg, _ := e.quantileAggregators.LoadOrStore(quantile, aggregator.NewQuantileAggregator(quantile))
	return agg.(aggregator.Aggregator)
}

func (e *KaitoScaler) IsActive(ctx context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	inferenceSet := &kaitov1beta1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	condition := metav1.Condition{}
	for i := range inferenceSet.Status.Conditions {
		if inferenceSet.Status.Conditions[i].Type == string(kaitov1beta1.InferenceSetConditionTypeReady) {
			condition = inferenceSet.Status.Conditions[i]
			break
		}
	}

	return &externalscaler.IsActiveResponse{
		Result: condition.Status == metav1.ConditionTrue,
	}, nil
}

func (e *KaitoScaler) StreamIsActive(sor *externalscaler.ScaledObjectRef, server externalscaler.ExternalScaler_StreamIsActiveServer) error {
	// keda-kaito-scaler does not support KEDA's external-push trigger. Returning
	// Unimplemented immediately surfaces a misconfiguration to the user, instead
	// of letting the KEDA push client get stuck in an infinite reconnect loop
	// (which is what would happen if we returned nil – the client treats nil as
	// io.EOF and keeps re-establishing the stream).
	return status.Error(codes.Unimplemented, "keda-kaito-scaler does not support push mode; use the regular external trigger")
}

func (e *KaitoScaler) GetMetricSpec(_ context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.GetMetricSpecResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	return &externalscaler.GetMetricSpecResponse{
		MetricSpecs: []*externalscaler.MetricSpec{{
			MetricName: scalerConfig.MetricName,
			// TargetSize (int64) is deprecated in the externalscaler proto in favor
			// of TargetSizeFloat. Using the float field also lets users express
			// sub-integer per-replica thresholds (e.g. 0.5 QPS).
			TargetSizeFloat: scalerConfig.Threshold,
		}},
	}, nil
}

func (e *KaitoScaler) GetMetrics(ctx context.Context, gmr *externalscaler.GetMetricsRequest) (*externalscaler.GetMetricsResponse, error) {
	scalerConfig, err := parseScalerMetadata(gmr.ScaledObjectRef, gmr.MetricName)
	if err != nil {
		return nil, err
	}

	inferenceSet := &kaitov1beta1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	// The readiness gate needs no scrape: it reports 1 while the InferenceSet has
	// replicas that are not yet ready (readyReplicas < desired spec replicas) and
	// 0 otherwise. Comparing against the desired replicas (spec) rather than the
	// current status replicas also catches the window where a scale-up was just
	// requested but the new workspace has not been created yet. Composite formulas
	// use it to avoid scaling on metrics gathered from a partially-ready fleet.
	if scalerConfig.Aggregation == AggregationGate {
		// Spec.Replicas is a pointer with a server-side default of 1; treat an
		// unset value as that default.
		desired := 1
		if inferenceSet.Spec.Replicas != nil {
			desired = int(*inferenceSet.Spec.Replicas)
		}
		value := 0.0
		if inferenceSet.Status.ReadyReplicas < desired {
			value = 1
		}
		klog.V(4).Infof("readiness gate for InferenceSet %s/%s: %f (ready=%d, desired=%d)",
			scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value,
			inferenceSet.Status.ReadyReplicas, desired)
		return &externalscaler.GetMetricsResponse{
			MetricValues: []*externalscaler.MetricValue{
				{
					MetricName:       scalerConfig.MetricName,
					MetricValueFloat: value,
				},
			},
		}, nil
	}

	scraper, ok := e.scrapers[scalerConfig.MetricSource]
	if !ok || scraper == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown metric source %q", scalerConfig.MetricSource))
	}

	// The quantile aggregation is parametric (target quantile comes from
	// metadata), so it is not held in the static aggregators map; instead it is
	// fetched from a per-quantile cache that reuses one immutable aggregator per
	// distinct quantile value.
	var agg aggregator.Aggregator
	if scalerConfig.Aggregation == AggregationQuantile {
		agg = e.quantileAggregator(scalerConfig.Quantile)
	} else {
		agg, ok = e.aggregators[scalerConfig.Aggregation]
		if !ok || agg == nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown aggregation %q", scalerConfig.Aggregation))
		}
	}

	// Take a per-call snapshot of metrics across every service belonging to this
	// InferenceSet. The snapshot is produced by a pluggable scraper.Scraper so
	// alternative sources (e.g. EndpointPicker, Prometheus queries) can be
	// swapped in without touching the scaler protocol logic below.
	snapshot, err := scraper.Scrape(ctx, inferenceSet, scalerConfig.scrapeConfig())
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to scrape metrics for InferenceSet %s/%s: %v", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, err))
	}
	klog.V(6).Infof("scraped snapshot for InferenceSet %s/%s with %d service(s)", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, len(snapshot.Services))

	value, err := agg.Aggregate(snapshot, scalerConfig.MetricName, scalerConfig.Threshold)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(4).Infof("aggregated metric %q for InferenceSet %s/%s: %f", scalerConfig.MetricName, scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)

	return &externalscaler.GetMetricsResponse{
		MetricValues: []*externalscaler.MetricValue{
			{
				MetricName:       scalerConfig.MetricName,
				MetricValueFloat: value,
			},
		},
	}, nil
}

func parseScalerMetadata(sor *externalscaler.ScaledObjectRef, metricName string) (*Config, error) {
	md := sor.ScalerMetadata

	// Mandatory: identifies the workload to scrape.
	inferenceSetName := md[InferenceSetNameInMetadata]
	if inferenceSetName == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set name must be specified")
	}
	inferenceSetNamespace := md[InferenceSetNamespaceInMetadata]
	if inferenceSetNamespace == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set namespace must be specified")
	}

	// metricName: GetMetrics receives it via the gRPC request (after KEDA strips
	// the sX- prefix); GetMetricSpec/IsActive pass "" and we fall back to
	// metadata. Either way it must end up non-empty.
	if metricName == "" {
		metricName = md[MetricNameInMetadata]
	}
	if metricName == "" {
		return nil, status.Error(codes.InvalidArgument, "metric name must be specified")
	}

	// Threshold is the per-replica target; required so users always make an
	// explicit scaling decision.
	thresholdStr := md[ThresholdInMetadata]
	if thresholdStr == "" {
		return nil, status.Error(codes.InvalidArgument, "threshold must be specified")
	}
	threshold, err := strconv.ParseFloat(thresholdStr, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "threshold must be a valid number")
	}

	// Optional scrape settings: default to Kaito's current vLLM exposure
	// (http on workspace Service port 80, /metrics, 3s timeout).
	metricProtocol := md[MetricProtocolInMetadata]
	if metricProtocol == "" {
		metricProtocol = defaultMetricProtocol
	} else if metricProtocol != "http" && metricProtocol != "https" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be either http or https")
	}

	metricPort := md[MetricPortInMetadata]
	if metricPort == "" {
		metricPort = defaultMetricPort
	}

	metricPath := md[MetricPathInMetadata]
	if metricPath == "" {
		metricPath = defaultMetricPath
	}

	scrapeTimeout := defaultScrapeTimeout
	if s := md[ScrapeTimeoutInMetadata]; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "scrape timeout must be a valid duration")
		}
		scrapeTimeout = d
	}

	// Optional routing: which scraper/aggregator serve this trigger. Default to
	// the legacy service+sum path so existing single-metric ScaledObjects behave
	// exactly as before.
	metricSource := md[MetricSourceInMetadata]
	if metricSource == "" {
		metricSource = defaultMetricSource
	}
	aggregation := md[AggregationInMetadata]
	if aggregation == "" {
		aggregation = defaultAggregation
	}

	// Optional target quantile for the "quantile" aggregation (default p95).
	quantile := defaultQuantile
	if q := md[QuantileInMetadata]; q != "" {
		v, err := strconv.ParseFloat(q, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "quantile must be a valid number")
		}
		if v <= 0 || v > 1 {
			return nil, status.Error(codes.InvalidArgument, "quantile must be in the (0, 1] range")
		}
		quantile = v
	}

	return &Config{
		InferenceSetName:      inferenceSetName,
		InferenceSetNamespace: inferenceSetNamespace,
		MetricName:            metricName,
		MetricProtocol:        metricProtocol,
		MetricPort:            metricPort,
		MetricPath:            metricPath,
		ScrapeTimeout:         scrapeTimeout,
		Threshold:             threshold,
		MetricSource:          metricSource,
		Aggregation:           aggregation,
		Quantile:              quantile,
	}, nil
}
