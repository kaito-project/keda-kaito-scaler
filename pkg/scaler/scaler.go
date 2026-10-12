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

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
	inferencesetutil "github.com/kaito-project/keda-kaito-scaler/pkg/util/inferenceset"
)

const (
	ScalerName = "keda-kaito-scaler"

	reasonMetricsUnavailable      = "MetricsUnavailable"
	reasonMetricAggregationFailed = "MetricAggregationFailed"
	reasonEPPPodsUnavailable      = "EPPPodsUnavailable"

	// Defaults applied when the corresponding metadata key is omitted. Only
	// inferenceSetName/inferenceSetNamespace/metricName remain always mandatory;
	// threshold is required only for aggregations that consume it (see
	// thresholdOptional). Everything else falls back to a sensible value matching
	// Kaito's current vLLM exposure conventions.
	defaultMetricProtocol = "http"
	defaultMetricPort     = "80"
	defaultMetricPath     = "/metrics"
	defaultScrapeTimeout  = 3 * time.Second

	// defaultMetricCacheWindow is the rolling window used by the windowed-avg
	// aggregation when the metricCacheWindow metadata key is omitted (5 minutes).
	defaultMetricCacheWindow = 5 * time.Minute

	// defaultThreshold is used when the aggregation does not consume a per-replica
	// threshold (service-avg/windowed-avg/gate); it is a placeholder KEDA overrides
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
	// MetricCacheWindow is the rolling window for the "windowed-avg" aggregation.
	MetricCacheWindow time.Duration
	// ZeroReplicaFallback makes this trigger report 0 instead of scraping while
	// the InferenceSet is parked or provisioning without a model Service. Set
	// only on modelpod triggers of a ScaledObject whose minimum is 0.
	ZeroReplicaFallback bool
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

// KaitoScaler implements the KEDA external scaler gRPC contract. Metric values
// are served from an in-memory MetricCache that a background poller keeps fresh
// (so every replica can answer from a complete window), and reduced to the
// single value KEDA expects by pluggable aggregator.Aggregators. A trigger
// selects its aggregator via the aggregation metadata key; the readiness gate is
// the only aggregation handled without an aggregators-map entry.
type KaitoScaler struct {
	kubeClient    client.Client
	serviceReader client.Reader
	aggregators   map[string]aggregator.Aggregator
	cache         *MetricCache
	recorder      record.EventRecorder
	externalscaler.UnimplementedExternalScalerServer
}

// NewKaitoScaler wires the Kubernetes client, the metric cache, and the set of
// named aggregators used to serve KEDA scaling requests. The caller registers
// every aggregation in the aggregators map, including the "windowed-avg"
// aggregation (served by the cache itself); see cmd/app/manager.go.
func NewKaitoScaler(kubeClient client.Client, cache *MetricCache, aggregators map[string]aggregator.Aggregator) *KaitoScaler {
	return NewKaitoScalerWithAPIReader(kubeClient, kubeClient, cache, aggregators)
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get

// NewKaitoScalerWithAPIReader uses apiReader for uncached Service lookups. This
// avoids starting a Service informer for a point lookup that only needs get RBAC.
func NewKaitoScalerWithAPIReader(kubeClient client.Client, apiReader client.Reader, cache *MetricCache, aggregators map[string]aggregator.Aggregator) *KaitoScaler {
	return NewKaitoScalerWithAPIReaderAndRecorder(kubeClient, apiReader, cache, aggregators, nil)
}

// NewKaitoScalerWithAPIReaderAndRecorder additionally emits Kubernetes Events
// for runtime metric failures that cannot be caught during provisioning.
func NewKaitoScalerWithAPIReaderAndRecorder(kubeClient client.Client, apiReader client.Reader, cache *MetricCache, aggregators map[string]aggregator.Aggregator, recorder record.EventRecorder) *KaitoScaler {
	return &KaitoScaler{
		kubeClient:    kubeClient,
		serviceReader: apiReader,
		cache:         cache,
		aggregators:   aggregators,
		recorder:      recorder,
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

	// The readiness gate and replica count need no scrape; both derive their
	// value from the InferenceSet object itself.
	if scalerConfig.Aggregation == constants.AggregationGate || scalerConfig.Aggregation == constants.AggregationReplicas {
		inferenceSet, err := e.getInferenceSet(ctx, scalerConfig)
		if err != nil {
			return nil, err
		}
		var value float64
		if scalerConfig.Aggregation == constants.AggregationGate {
			value = readinessGateValue(inferenceSet)
			klog.V(4).Infof("readiness gate for InferenceSet %s/%s: %f", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)
		} else {
			value = float64(desiredReplicas(inferenceSet))
			klog.V(4).Infof("replica count for InferenceSet %s/%s: %f", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)
		}
		return newMetricValueResponse(scalerConfig.MetricName, value), nil
	}

	// A parked or still-provisioning InferenceSet has no model Service to scrape,
	// so a modelpod metric would otherwise report a scrape failure and freeze the
	// composite formula. Report 0 until the Service exists. The scale-to-zero
	// formula separately requires every activation signal to be idle, so queued
	// EPP work vetoes parking while the backend is provisioning.
	//
	// Gated on the trigger's own opt-in rather than inferred, so a ScaledObject
	// that cannot reach zero keeps treating a failed scrape as an error.
	if scalerConfig.ZeroReplicaFallback && scalerConfig.MetricSource == metricsource.ModelPodSourceName {
		inferenceSet, err := e.getInferenceSet(ctx, scalerConfig)
		if err != nil {
			return nil, err
		}
		if desiredReplicas(inferenceSet) == 0 {
			klog.V(4).Infof("InferenceSet %s/%s is at zero replicas; reporting 0 for metric %q without scraping",
				scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, scalerConfig.MetricName)
			return newMetricValueResponse(scalerConfig.MetricName, 0), nil
		}
		hasService, err := e.hasModelService(ctx, inferenceSet)
		if err != nil {
			return nil, err
		}
		if !hasService {
			klog.V(4).Infof("InferenceSet %s/%s has no model Service yet; reporting 0 for metric %q without scraping",
				scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, scalerConfig.MetricName)
			return newMetricValueResponse(scalerConfig.MetricName, 0), nil
		}
	}

	if !e.cache.hasSource(scalerConfig.MetricSource) {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown metric source %q", scalerConfig.MetricSource))
	}

	agg, ok := e.aggregators[scalerConfig.Aggregation]
	if !ok || agg == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown aggregation %q", scalerConfig.Aggregation))
	}

	is := types.NamespacedName{Namespace: scalerConfig.InferenceSetNamespace, Name: scalerConfig.InferenceSetName}
	scrapeCfg := scalerConfig.scrapeConfig()

	// Every metric is served from the background cache (no live scrape): Current
	// registers the target so the poller keeps it fresh and returns its newest
	// snapshot. A cold cache (no snapshot yet) or a stale one (scraping has been
	// failing) is reported as unavailable so KEDA/HPA holds instead of scaling on
	// missing or outdated data. The windowed-avg aggregation additionally uses the
	// target's snapshot window (via the cache, which is registered as its
	// aggregator).
	snapshot, ok := e.cache.Current(ctx, is, scrapeCfg, scalerConfig.MetricSource, scalerConfig.MetricCacheWindow)
	if !ok {
		e.eventf(scalerConfig, reasonMetricsUnavailable,
			"Metrics for metric %q from source %q are unavailable (scrape failing or cache cold)",
			scalerConfig.MetricName, scalerConfig.MetricSource)
		return nil, status.Error(codes.Unavailable, fmt.Sprintf("metrics for InferenceSet %s/%s are not available yet (scrape failing or cold)", scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName))
	}
	if scalerConfig.MetricSource == metricsource.EPPSourceName &&
		scalerConfig.Aggregation == aggregator.SumAggregatorName && len(snapshot.Services) == 0 {
		e.eventf(scalerConfig, reasonEPPPodsUnavailable,
			"No ready Endpoint Picker pods available for selector %s in namespace %s; this is expected temporarily during startup while KAITO creates the first Workspace and Endpoint Picker",
			metricsource.EPPSelectorDescription(snapshot.InferenceSet.Name), snapshot.InferenceSet.Namespace)
		return nil, status.Error(codes.Internal, fmt.Sprintf(
			"no ready Endpoint Picker pods available for selector %s in namespace %s; "+
				"this is expected temporarily during startup while KAITO creates the first Workspace and Endpoint Picker; "+
				"if it persists, verify the InferenceSet uses a vLLM preset and the Gateway API Inference Extension is enabled",
			metricsource.EPPSelectorDescription(snapshot.InferenceSet.Name), snapshot.InferenceSet.Namespace))
	}

	aggregationThreshold := scalerConfig.Threshold
	if scalerConfig.MetricSource == metricsource.EPPSourceName && scalerConfig.Aggregation == aggregator.SumAggregatorName {
		aggregationThreshold = 0
	}
	value, err := agg.Aggregate(snapshot, aggregator.AggregateInput{
		MetricName:   scalerConfig.MetricName,
		Threshold:    aggregationThreshold,
		InferenceSet: is,
		MetricSource: scalerConfig.MetricSource,
		ScrapeConfig: scrapeCfg,
		Window:       scalerConfig.MetricCacheWindow,
	})
	if err != nil {
		e.eventf(scalerConfig, reasonMetricAggregationFailed,
			"Failed to aggregate metric %q using %q: %v",
			scalerConfig.MetricName, scalerConfig.Aggregation, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(4).Infof("aggregated metric %q for InferenceSet %s/%s: %f", scalerConfig.MetricName, scalerConfig.InferenceSetNamespace, scalerConfig.InferenceSetName, value)

	return newMetricValueResponse(scalerConfig.MetricName, value), nil
}

func (e *KaitoScaler) eventf(cfg *Config, reason, messageFmt string, args ...any) {
	if e.recorder == nil {
		return
	}
	e.recorder.Eventf(&kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.InferenceSetName,
			Namespace: cfg.InferenceSetNamespace,
		},
	}, corev1.EventTypeWarning, reason, messageFmt, args...)
}

func (e *KaitoScaler) hasModelService(ctx context.Context, inferenceSet *kaitov1beta1.InferenceSet) (bool, error) {
	workspaces, err := inferencesetutil.ListWorkspaces(ctx, inferenceSet, e.kubeClient)
	if err != nil {
		return false, fmt.Errorf("failed to list workspaces for InferenceSet %s/%s: %w", inferenceSet.Namespace, inferenceSet.Name, err)
	}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		service := &corev1.Service{}
		err := e.serviceReader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, service)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get model Service %s/%s: %w", workspace.Namespace, workspace.Name, err)
		}
	}
	return false, nil
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

	// Threshold is the positive target returned to KEDA by GetMetricSpec. It is
	// required for the legacy modelpod+sum path, but optional for composite
	// triggers whose target is replaced by scalingModifiers. EPP+sum also uses
	// the positive default here; GetMetrics separately passes 0 to SumAggregator
	// to disable missing-service compensation. A supplied value is always
	// validated.
	threshold := defaultThreshold
	if thresholdStr := md[constants.ThresholdInMetadata]; thresholdStr != "" {
		v, err := strconv.ParseFloat(thresholdStr, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "threshold must be a valid number")
		}
		threshold = v
	} else if !(metricSource == metricsource.EPPSourceName && aggregation == aggregator.SumAggregatorName) && !thresholdOptional(aggregation) {
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

	// Optional cache window for the "windowed-avg" aggregation (bare seconds,
	// default 5 minutes). It must be a positive integer number of seconds.
	metricCacheWindow := defaultMetricCacheWindow
	if w := md[constants.MetricCacheWindowInMetadata]; w != "" {
		secs, err := strconv.Atoi(w)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "metric cache window must be an integer number of seconds")
		}
		if secs <= 0 {
			return nil, status.Error(codes.InvalidArgument, "metric cache window must be a positive number of seconds")
		}
		metricCacheWindow = time.Duration(secs) * time.Second
	}

	// Internal opt-in emitted by the provisioner for modelpod triggers that can
	// reach zero replicas. Only the literal "true" enables the fallback.
	zeroReplicaFallback := md[constants.ZeroReplicaFallbackInMetadata] == "true"

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
		MetricCacheWindow:     metricCacheWindow,
		ZeroReplicaFallback:   zeroReplicaFallback,
	}, nil
}

// thresholdOptional reports whether the aggregation ignores the per-replica
// threshold, so callers need not supply it in the trigger metadata.
func thresholdOptional(aggregation string) bool {
	switch aggregation {
	case aggregator.ServiceAverageAggregatorName,
		constants.AggregationWindowedAvg, constants.AggregationGate, constants.AggregationReplicas:
		return true
	default:
		return false
	}
}

// getInferenceSet fetches the InferenceSet a trigger refers to. Shared by the
// status-derived aggregations and the zero-replica short-circuit, all of which
// answer from the object rather than a scrape.
func (e *KaitoScaler) getInferenceSet(ctx context.Context, cfg *Config) (*kaitov1beta1.InferenceSet, error) {
	inferenceSet := &kaitov1beta1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: cfg.InferenceSetNamespace,
		Name:      cfg.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", cfg.InferenceSetName, cfg.InferenceSetNamespace, err))
	}
	return inferenceSet, nil
}

// desiredReplicas returns the InferenceSet's desired replica count.
//
// It reads spec.replicas rather than status.readyReplicas deliberately: a
// mid-scale-up state (replicas=1, ready=0) must not look like zero replicas, or
// the formula would route back into the activation branch. spec.replicas is also
// what KEDA itself reads.
func desiredReplicas(is *kaitov1beta1.InferenceSet) int {
	// Defensive only: the field carries a server-side default of 1.
	if is.Spec.Replicas == nil {
		return 1
	}
	return int(*is.Spec.Replicas)
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
