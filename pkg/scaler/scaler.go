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
	ScalerNameKeyInMetadata         = "scalerName"
	InferenceSetNameInMetadata      = "inferenceSetName"
	InferenceSetNamespaceInMetadata = "inferenceSetNamespace"
	ScalerAddressInMetadata         = "scalerAddress"
	MetricNameInMetadata            = "metricName"
	ThresholdInMetadata             = "threshold"
	MetricProtocolInMetadata        = "metricProtocol"
	MetricPortInMetadata            = "metricPort"
	MetricPathInMetadata            = "metricPath"
	ScrapeTimeoutInMetadata         = "scrapeTimeout"
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
	Threshold             int64
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
	// do nothing for stream mode
	return nil
}

func (e *KaitoScaler) GetMetricSpec(_ context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.GetMetricSpecResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	return &externalscaler.GetMetricSpecResponse{
		MetricSpecs: []*externalscaler.MetricSpec{{
			MetricName: scalerConfig.MetricName,
			TargetSize: scalerConfig.Threshold,
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
	if scalerName, ok := sor.ScalerMetadata[ScalerNameKeyInMetadata]; !ok || scalerName != ScalerName {
		return nil, status.Error(codes.InvalidArgument, "scaler name must be keda-kaito-scaler")
	}

	inferenceSetName, ok := sor.ScalerMetadata[InferenceSetNameInMetadata]
	if !ok || inferenceSetName == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set name must be specified")
	}

	inferenceSetNamespace, ok := sor.ScalerMetadata[InferenceSetNamespaceInMetadata]
	if !ok || inferenceSetNamespace == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set namespace must be specified")
	}

	if metricName == "" {
		name, ok := sor.ScalerMetadata[MetricNameInMetadata]
		if !ok || name == "" {
			return nil, status.Error(codes.InvalidArgument, "metric name must be specified")
		}
		metricName = name
	}

	metricProtocol, ok := sor.ScalerMetadata[MetricProtocolInMetadata]
	if !ok || metricProtocol == "" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be specified")
	} else if metricProtocol != "http" && metricProtocol != "https" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be either http or https")
	}

	metricPort, ok := sor.ScalerMetadata[MetricPortInMetadata]
	if !ok || metricPort == "" {
		return nil, status.Error(codes.InvalidArgument, "metric port must be specified")
	}

	metricPath, ok := sor.ScalerMetadata[MetricPathInMetadata]
	if !ok || metricPath == "" {
		return nil, status.Error(codes.InvalidArgument, "metric path must be specified")
	}

	scrapeTimeoutStr, ok := sor.ScalerMetadata[ScrapeTimeoutInMetadata]
	if !ok || scrapeTimeoutStr == "" {
		return nil, status.Error(codes.InvalidArgument, "scrape timeout must be specified")
	}

	scrapeTimeout, err := time.ParseDuration(scrapeTimeoutStr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "scrape timeout must be a valid duration")
	}

	thresholdStr, ok := sor.ScalerMetadata[ThresholdInMetadata]
	if !ok || thresholdStr == "" {
		return nil, status.Error(codes.InvalidArgument, "threshold must be specified")
	}

	threshold, err := strconv.ParseInt(thresholdStr, 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "threshold must be a valid integer")
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
