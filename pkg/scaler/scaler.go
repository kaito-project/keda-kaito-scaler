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
	"time"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metrics"
)

const (
	ScalerName = "keda-kaito-scaler"

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

	// Defaults applied when the corresponding metadata key is omitted. Only
	// inferenceSetName/inferenceSetNamespace/metricName/threshold remain
	// mandatory; everything else falls back to a sensible value matching
	// Kaito's current vLLM exposure conventions.
	defaultMetricProtocol = "http"
	defaultMetricPort     = "80"
	defaultMetricPath     = "/metrics"
	defaultScrapeTimeout  = 3 * time.Second
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
}

// scrapeConfig projects the subset of Config needed by the metrics Scraper.
func (c *Config) scrapeConfig() metrics.ScrapeConfig {
	return metrics.ScrapeConfig{
		Protocol: c.MetricProtocol,
		Port:     c.MetricPort,
		Path:     c.MetricPath,
		Timeout:  c.ScrapeTimeout,
	}
}

// KaitoScaler implements the KEDA external scaler gRPC contract on top of a
// pluggable metrics.Scraper (which fetches raw per-service metrics) and a
// metrics.Aggregator (which reduces them to the single value KEDA expects).
type KaitoScaler struct {
	kubeClient client.Client
	scraper    metrics.Scraper
	aggregator metrics.Aggregator
	externalscaler.UnimplementedExternalScalerServer
}

// NewKaitoScaler wires the Kubernetes client, the metrics scraper and the
// aggregator that will be used to serve KEDA scaling requests.
func NewKaitoScaler(kubeClient client.Client, scraper metrics.Scraper, aggregator metrics.Aggregator) *KaitoScaler {
	return &KaitoScaler{
		kubeClient: kubeClient,
		scraper:    scraper,
		aggregator: aggregator,
	}
}

func (e *KaitoScaler) IsActive(ctx context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	inferenceSet := &kaitov1alpha1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	condition := metav1.Condition{}
	for i := range inferenceSet.Status.Conditions {
		if inferenceSet.Status.Conditions[i].Type == string(kaitov1alpha1.InferenceSetConditionTypeReady) {
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

	inferenceSet := &kaitov1alpha1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	// Take a per-call snapshot of metrics across every service belonging to this
	// InferenceSet. The snapshot is produced by a pluggable metrics.Scraper so
	// alternative sources (e.g. Pod direct-scraping, Prometheus queries) can be
	// swapped in without touching the scaler protocol logic below.
	snapshot, err := e.scraper.Scrape(ctx, inferenceSet, scalerConfig.scrapeConfig())
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to scrape metrics for InferenceSet %s/%s: %v", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, err))
	}
	klog.V(6).Infof("scraped snapshot for InferenceSet %s/%s with %d service(s)", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, len(snapshot.Services))

	value, err := e.aggregator.Aggregate(snapshot, scalerConfig.MetricName, scalerConfig.Threshold)
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

	return &Config{
		InferenceSetName:      inferenceSetName,
		InferenceSetNamespace: inferenceSetNamespace,
		MetricName:            metricName,
		MetricProtocol:        metricProtocol,
		MetricPort:            metricPort,
		MetricPath:            metricPath,
		ScrapeTimeout:         scrapeTimeout,
		Threshold:             threshold,
	}, nil
}
