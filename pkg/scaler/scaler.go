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
	"math"
	"strconv"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

const (
	ScalerName = "keda-kaito-scaler"

	// Defaults applied when the corresponding metadata key is omitted. Only
	// inferenceSetName/inferenceSetNamespace/metricName remain always mandatory;
	// threshold is required only for aggregations that consume it (see
	// thresholdOptional). Everything else falls back to a sensible value matching
	// Kaito's current vLLM exposure conventions.
	defaultMetricProtocol = "http"
	defaultMetricPort     = "80"
	defaultMetricPath     = "/metrics"
	defaultScrapeTimeout  = 3 * time.Second

	// defaultQuantile is the target quantile used by the "quantile" aggregation
	// when the quantile metadata key is omitted (p95).
	defaultQuantile = 0.95

	// defaultThreshold is used when the aggregation does not consume a per-replica
	// threshold (service-avg/quantile/gate); it is a placeholder KEDA overrides
	// via the composite scalingModifiers formula.
	defaultThreshold = 1.0
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
	// MetricSource / Aggregation select the metric source and aggregator used to serve
	// this trigger. They default to "service"/"sum" for backward compatibility.
	MetricSource string
	Aggregation  string
	// Quantile is the target quantile in (0, 1] for the "quantile" aggregation.
	Quantile float64
}

// scrapeConfig projects the subset of Config needed by the metric source.
func (c *Config) scrapeConfig() metricsource.ScrapeConfig {
	return metricsource.ScrapeConfig{
		Protocol: c.MetricProtocol,
		Port:     c.MetricPort,
		Path:     c.MetricPath,
		Timeout:  c.ScrapeTimeout,
	}
}

// KaitoScaler implements the KEDA external scaler gRPC contract on top of
// pluggable metricsource.MetricSources (which fetch raw per-service metrics) and
// aggregator.Aggregators (which reduce them to the single value KEDA expects).
// A trigger selects its metric source/aggregator via the metricSource/aggregation
// metadata keys, letting one scaler serve both the legacy single-metric path
// and the new composite (queue length + latency + readiness gate) triggers.
type KaitoScaler struct {
	kubeClient    client.Client
	metricSources map[string]metricsource.MetricSource
	aggregators   map[string]aggregator.Aggregator
	externalscaler.UnimplementedExternalScalerServer
}

// NewKaitoScaler wires the Kubernetes client, the set of named metric sources
// and the set of named aggregators used to serve KEDA scaling requests. The
// maps must at least contain the "modelpod" metric source and the "sum" aggregator so
// the default single-metric path keeps working.
func NewKaitoScaler(kubeClient client.Client, metricSources map[string]metricsource.MetricSource, aggregators map[string]aggregator.Aggregator) *KaitoScaler {
	return &KaitoScaler{
		kubeClient:    kubeClient,
		metricSources: metricSources,
		aggregators:   aggregators,
	}
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

	// The readiness gate needs no scrape; it derives its value from the
	// InferenceSet's replica readiness.
	if scalerConfig.Aggregation == constants.AggregationGate {
		value := readinessGateValue(inferenceSet)
		klog.V(4).Infof("readiness gate for InferenceSet %s/%s: %f", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)
		return newMetricValueResponse(scalerConfig.MetricName, value), nil
	}

	source, ok := e.metricSources[scalerConfig.MetricSource]
	if !ok || source == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown metric source %q", scalerConfig.MetricSource))
	}

	agg, ok := e.aggregators[scalerConfig.Aggregation]
	if !ok || agg == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown aggregation %q", scalerConfig.Aggregation))
	}

	// Take a per-call snapshot of metrics across every service belonging to this
	// InferenceSet. The snapshot is produced by a pluggable metricsource.MetricSource so
	// alternative sources (e.g. EndpointPicker, Prometheus queries) can be
	// swapped in without touching the scaler protocol logic below.
	snapshot, err := source.Scrape(ctx, inferenceSet, scalerConfig.scrapeConfig())
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to scrape metrics for InferenceSet %s/%s: %v", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, err))
	}
	klog.V(6).Infof("scraped snapshot for InferenceSet %s/%s with %d service(s)", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, len(snapshot.Services))

	value, err := agg.Aggregate(snapshot, aggregator.AggregateInput{
		MetricName: scalerConfig.MetricName,
		Threshold:  scalerConfig.Threshold,
		Quantile:   scalerConfig.Quantile,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(4).Infof("aggregated metric %q for InferenceSet %s/%s: %f", scalerConfig.MetricName, scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)

	return newMetricValueResponse(scalerConfig.MetricName, value), nil
}

func parseScalerMetadata(sor *externalscaler.ScaledObjectRef, metricName string) (*Config, error) {
	md := sor.ScalerMetadata

	// Mandatory: identifies the workload to scrape.
	inferenceSetName := md[constants.InferenceSetNameInMetadata]
	if inferenceSetName == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set name must be specified")
	}
	inferenceSetNamespace := md[constants.InferenceSetNamespaceInMetadata]
	if inferenceSetNamespace == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set namespace must be specified")
	}

	// metricName: GetMetrics receives it via the gRPC request (after KEDA strips
	// the sX- prefix); GetMetricSpec/IsActive pass "" and we fall back to
	// metadata. Either way it must end up non-empty.
	if metricName == "" {
		metricName = md[constants.MetricNameInMetadata]
	}
	if metricName == "" {
		return nil, status.Error(codes.InvalidArgument, "metric name must be specified")
	}

	// Routing: which metric source/aggregator serve this trigger. Default to the
	// legacy modelpod+sum path so existing single-metric ScaledObjects behave the
	// same as before.
	metricSource := md[constants.MetricSourceInMetadata]
	if metricSource == "" {
		metricSource = metricsource.ModelPodSourceName
	}
	aggregation := md[constants.AggregationInMetadata]
	if aggregation == "" {
		aggregation = aggregator.SumAggregatorName
	}

	// Threshold is the per-replica HPA target. It is required for aggregations
	// that consume it (the single-metric "sum" path, where KEDA uses it as the
	// AverageValue target). For service-avg/quantile/gate it is optional and
	// defaults to defaultThreshold, since those run in composite Value mode where
	// the per-trigger target is overridden by the scalingModifiers formula. A
	// supplied value is always validated.
	threshold := defaultThreshold
	if thresholdStr := md[constants.ThresholdInMetadata]; thresholdStr != "" {
		v, err := strconv.ParseFloat(thresholdStr, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "threshold must be a valid number")
		}
		threshold = v
	} else if !thresholdOptional(aggregation) {
		return nil, status.Error(codes.InvalidArgument, "threshold must be specified")
	}

	// Optional scrape settings: default to Kaito's current vLLM exposure
	// (http on workspace Service port 80, /metrics, 3s timeout).
	metricProtocol := md[constants.MetricProtocolInMetadata]
	if metricProtocol == "" {
		metricProtocol = defaultMetricProtocol
	} else if metricProtocol != "http" && metricProtocol != "https" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be either http or https")
	}

	metricPort := md[constants.MetricPortInMetadata]
	if metricPort == "" {
		metricPort = defaultMetricPort
	}

	metricPath := md[constants.MetricPathInMetadata]
	if metricPath == "" {
		metricPath = defaultMetricPath
	}

	scrapeTimeout := defaultScrapeTimeout
	if s := md[constants.ScrapeTimeoutInMetadata]; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "scrape timeout must be a valid duration")
		}
		scrapeTimeout = d
	}

	// Optional target quantile for the "quantile" aggregation (default p95).
	quantile := defaultQuantile
	if q := md[constants.QuantileInMetadata]; q != "" {
		v, err := strconv.ParseFloat(q, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "quantile must be a valid number")
		}
		if math.IsNaN(v) || v <= 0 || v > 1 {
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

// thresholdOptional reports whether the aggregation ignores the per-replica
// threshold, so callers need not supply it in the trigger metadata.
func thresholdOptional(aggregation string) bool {
	switch aggregation {
	case aggregator.ServiceAverageAggregatorName, aggregator.QuantileAggregatorName, constants.AggregationGate:
		return true
	default:
		return false
	}
}

// readinessGateValue returns 1 when every desired replica is ready
// (readyReplicas >= desired spec replicas) and 0 while some are still not ready.
// Comparing against the desired (spec) replicas also catches a just-requested
// scale-up whose workspace does not exist yet, letting composite formulas avoid
// scaling on metrics from a partially-ready fleet.
func readinessGateValue(is *kaitov1beta1.InferenceSet) float64 {
	// Spec.Replicas is a pointer with a server-side default of 1.
	desired := 1
	if is.Spec.Replicas != nil {
		desired = int(*is.Spec.Replicas)
	}
	if is.Status.ReadyReplicas < desired {
		return 0
	}
	return 1
}

// newMetricValueResponse builds the single-value GetMetricsResponse KEDA expects.
func newMetricValueResponse(name string, value float64) *externalscaler.GetMetricsResponse {
	return &externalscaler.GetMetricsResponse{
		MetricValues: []*externalscaler.MetricValue{
			{MetricName: name, MetricValueFloat: value},
		},
	}
}
