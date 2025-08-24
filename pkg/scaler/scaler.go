package scaler

import (
	"context"

	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
)

const (
	ScalerName = "keda-kaito-scaler"

	// Metadata keys
	ScalerNameKeyInMetadata      = "scalerName"
	WorkspaceNameInMetadata      = "workspaceName"
	WorkspaceNamespaceInMetadata = "workspaceNamespace"
	ScalerAddressInMetadata      = "scalerAddress"
	MetricNameInMetadata         = "metricName"
	ThresholdInMetadata          = "threshold"
	MetricProtocolInMetadata     = "metricProtocol"
	MetricPortInMetadata         = "metricPort"
	MetricPathInMetadata         = "metricPath"
	ScrapeTimeoutInMetadata      = "scrapeTimeout"
)

type kaitoScaler struct {
	externalscaler.UnimplementedExternalScalerServer
}

func NewKaitoScaler() *kaitoScaler {
	return &kaitoScaler{}
}

func (e *kaitoScaler) IsActive(ctx context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	// Implement the logic to determine if the scaler is active
	// This could involve checking the current state of the resource being scaled
	// and returning true if scaling is needed, false otherwise.
	return &externalscaler.IsActiveResponse{}, nil
}

func (e *kaitoScaler) StreamIsActive(sor *externalscaler.ScaledObjectRef, server externalscaler.ExternalScaler_StreamIsActiveServer) error {
	// Implement the logic to stream the active status of the scaler
	// This could involve periodically checking the state of the resource being scaled
	// and sending updates to the client.
	return nil
}

func (e *kaitoScaler) GetMetricSpec(_ context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.GetMetricSpecResponse, error) {
	// Implement the logic to retrieve the metric specification for the scaler
	// This could involve defining the metrics that the scaler will use to make scaling decisions.
	return &externalscaler.GetMetricSpecResponse{}, nil
}

func (e *kaitoScaler) GetMetrics(context.Context, *externalscaler.GetMetricsRequest) (*externalscaler.GetMetricsResponse, error) {
	// Implement the logic to retrieve the current metrics for the scaler
	// This could involve querying the resource being scaled for its current state.
	return &externalscaler.GetMetricsResponse{}, nil
}
