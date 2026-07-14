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
	"errors"
	"testing"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

// stubSource is a test double for metricsource.MetricSource.
type stubSource struct {
	snapshot *metricsource.MetricSnapshot
	err      error
	gotCfg   metricsource.ScrapeConfig
	gotIS    types.NamespacedName
}

func (s *stubSource) Name() string { return "stub-source" }

func (s *stubSource) Scrape(_ context.Context, is *kaitov1beta1.InferenceSet, cfg metricsource.ScrapeConfig) (*metricsource.MetricSnapshot, error) {
	s.gotCfg = cfg
	s.gotIS = types.NamespacedName{Namespace: is.Namespace, Name: is.Name}
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

// stubAggregator is a test double for aggregator.Aggregator.
type stubAggregator struct {
	value     float64
	err       error
	gotMetric string
	gotSnap   *metricsource.MetricSnapshot
	threshold float64
	callCount int
}

func (a *stubAggregator) Name() string { return "stub-aggregator" }

func (a *stubAggregator) Aggregate(snap *metricsource.MetricSnapshot, input aggregator.AggregateInput) (float64, error) {
	a.callCount++
	a.gotSnap = snap
	a.gotMetric = input.MetricName
	a.threshold = input.Threshold
	return a.value, a.err
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// newTestScaler wraps a single metric source/aggregator into the default
// "modelpod"/"sum" routing maps so existing single-metric tests keep working
// unchanged.
func newTestScaler(c client.Client, sc metricsource.MetricSource, ag aggregator.Aggregator) *KaitoScaler {
	return NewKaitoScaler(c,
		map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: sc},
		map[string]aggregator.Aggregator{aggregator.SumAggregatorName: ag},
	)
}

func newValidScalerMetadata() map[string]string {
	return map[string]string{
		constants.InferenceSetNameInMetadata:      "is1",
		constants.InferenceSetNamespaceInMetadata: "ns1",
		constants.MetricNameInMetadata:            "vllm:num_requests_waiting",
		constants.MetricProtocolInMetadata:        "http",
		constants.MetricPortInMetadata:            "80",
		constants.MetricPathInMetadata:            "/metrics",
		constants.ScrapeTimeoutInMetadata:         "3s",
		constants.ThresholdInMetadata:             "10",
	}
}

func newReadyInferenceSet(name, namespace string, ready bool) *kaitov1beta1.InferenceSet {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: kaitov1beta1.InferenceSetStatus{
			Conditions: []metav1.Condition{{
				Type:   string(kaitov1beta1.InferenceSetConditionTypeReady),
				Status: status,
			}},
		},
	}
}

func TestNewKaitoScaler(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	sc := &stubSource{}
	ag := &stubAggregator{}

	s := NewKaitoScaler(c,
		map[string]metricsource.MetricSource{"modelpod": sc},
		map[string]aggregator.Aggregator{"sum": ag},
	)

	assert.NotNil(t, s)
	assert.Equal(t, c, s.kubeClient)
	assert.Equal(t, metricsource.MetricSource(sc), s.metricSources["modelpod"])
	assert.Equal(t, aggregator.Aggregator(ag), s.aggregators["sum"])
}

func TestParseScalerMetadata(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(m map[string]string)
		metricName string
		wantErr    bool
	}{
		{name: "valid"},
		{name: "missing inference set name", mutate: func(m map[string]string) { delete(m, constants.InferenceSetNameInMetadata) }, wantErr: true},
		{name: "missing inference set namespace", mutate: func(m map[string]string) { delete(m, constants.InferenceSetNamespaceInMetadata) }, wantErr: true},
		{name: "missing metric name", mutate: func(m map[string]string) { delete(m, constants.MetricNameInMetadata) }, wantErr: true},
		{name: "invalid protocol", mutate: func(m map[string]string) { m[constants.MetricProtocolInMetadata] = "ftp" }, wantErr: true},
		{name: "invalid timeout", mutate: func(m map[string]string) { m[constants.ScrapeTimeoutInMetadata] = "abc" }, wantErr: true},
		{name: "invalid threshold", mutate: func(m map[string]string) { m[constants.ThresholdInMetadata] = "abc" }, wantErr: true},
		{name: "invalid quantile", mutate: func(m map[string]string) { m[constants.QuantileInMetadata] = "abc" }, wantErr: true},
		{name: "quantile out of range high", mutate: func(m map[string]string) { m[constants.QuantileInMetadata] = "1.5" }, wantErr: true},
		{name: "quantile out of range low", mutate: func(m map[string]string) { m[constants.QuantileInMetadata] = "0" }, wantErr: true},
		{name: "quantile NaN", mutate: func(m map[string]string) { m[constants.QuantileInMetadata] = "NaN" }, wantErr: true},
		{name: "metric name override", mutate: func(m map[string]string) { delete(m, constants.MetricNameInMetadata) }, metricName: "override:metric"},
		{name: "optional fields default when omitted", mutate: func(m map[string]string) {
			delete(m, constants.MetricProtocolInMetadata)
			delete(m, constants.MetricPortInMetadata)
			delete(m, constants.MetricPathInMetadata)
			delete(m, constants.ScrapeTimeoutInMetadata)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := newValidScalerMetadata()
			if tt.mutate != nil {
				tt.mutate(meta)
			}
			sor := &externalscaler.ScaledObjectRef{ScalerMetadata: meta}
			cfg, err := parseScalerMetadata(sor, tt.metricName)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, "is1", cfg.InferenceSetName)
			assert.Equal(t, "ns1", cfg.InferenceSetNamespace)
			assert.Equal(t, float64(10), cfg.Threshold)
			assert.Equal(t, 3*time.Second, cfg.ScrapeTimeout)
			assert.Equal(t, defaultQuantile, cfg.Quantile)
			if tt.metricName != "" {
				assert.Equal(t, tt.metricName, cfg.MetricName)
			}
		})
	}
}

func TestParseScalerMetadata_ThresholdOnDemand(t *testing.T) {
	tests := []struct {
		name        string
		aggregation string
		wantErr     bool
	}{
		{name: "required for default sum", aggregation: "", wantErr: true},
		{name: "required for sum", aggregation: aggregator.SumAggregatorName, wantErr: true},
		{name: "optional for service-avg", aggregation: aggregator.ServiceAverageAggregatorName},
		{name: "optional for quantile", aggregation: aggregator.QuantileAggregatorName},
		{name: "optional for gate", aggregation: constants.AggregationGate},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := newValidScalerMetadata()
			delete(meta, constants.ThresholdInMetadata)
			if tt.aggregation != "" {
				meta[constants.AggregationInMetadata] = tt.aggregation
			}
			sor := &externalscaler.ScaledObjectRef{ScalerMetadata: meta}
			cfg, err := parseScalerMetadata(sor, "")
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, defaultThreshold, cfg.Threshold)
		})
	}
}

func TestConfig_scrapeConfig(t *testing.T) {
	cfg := &Config{
		MetricProtocol: "https",
		MetricPort:     "8443",
		MetricPath:     "/metrics",
		ScrapeTimeout:  5 * time.Second,
	}
	sc := cfg.scrapeConfig()
	assert.Equal(t, "https", sc.Protocol)
	assert.Equal(t, "8443", sc.Port)
	assert.Equal(t, "/metrics", sc.Path)
	assert.Equal(t, 5*time.Second, sc.Timeout)
}

func TestKaitoScaler_IsActive(t *testing.T) {
	tests := []struct {
		name    string
		ready   bool
		missing bool
		want    bool
		wantErr bool
	}{
		{name: "ready true", ready: true, want: true},
		{name: "ready false", ready: false, want: false},
		{name: "inference set missing", missing: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if !tt.missing {
				objs = append(objs, newReadyInferenceSet("is1", "ns1", tt.ready))
			}
			s := newTestScaler(newFakeClient(t, objs...), &stubSource{}, &stubAggregator{})

			sor := &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()}
			resp, err := s.IsActive(context.Background(), sor)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, resp.Result)
		})
	}
}

func TestKaitoScaler_GetMetricSpec(t *testing.T) {
	s := newTestScaler(newFakeClient(t), &stubSource{}, &stubAggregator{})
	sor := &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()}
	resp, err := s.GetMetricSpec(context.Background(), sor)
	assert.NoError(t, err)
	assert.Len(t, resp.MetricSpecs, 1)
	assert.Equal(t, "vllm:num_requests_waiting", resp.MetricSpecs[0].MetricName)
	assert.Equal(t, float64(10), resp.MetricSpecs[0].TargetSizeFloat)
}

func TestKaitoScaler_GetMetrics(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)

	t.Run("delegates to metric source and aggregator", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{
			InferenceSet: types.NamespacedName{Namespace: "ns1", Name: "is1"},
			Services: []metricsource.ServiceMetrics{
				{Name: "ws0", Namespace: "ns1", Metrics: map[string]float64{"vllm:num_requests_waiting": 7}},
			},
		}
		sc := &stubSource{snapshot: snap}
		ag := &stubAggregator{value: 7}
		s := newTestScaler(newFakeClient(t, is), sc, ag)

		resp, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()},
			MetricName:      "vllm:num_requests_waiting",
		})
		assert.NoError(t, err)
		assert.Len(t, resp.MetricValues, 1)
		assert.Equal(t, float64(7), resp.MetricValues[0].MetricValueFloat)
		assert.Equal(t, "vllm:num_requests_waiting", resp.MetricValues[0].MetricName)
		assert.Equal(t, "http", sc.gotCfg.Protocol)
		assert.Equal(t, "80", sc.gotCfg.Port)
		assert.Equal(t, "/metrics", sc.gotCfg.Path)
		assert.Equal(t, 3*time.Second, sc.gotCfg.Timeout)
		assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "is1"}, sc.gotIS)
		assert.Equal(t, "vllm:num_requests_waiting", ag.gotMetric)
		assert.Equal(t, float64(10), ag.threshold)
		assert.Same(t, snap, ag.gotSnap)
		assert.Equal(t, 1, ag.callCount)
	})

	t.Run("metric source failure bubbles up", func(t *testing.T) {
		sc := &stubSource{err: errors.New("boom")}
		ag := &stubAggregator{}
		s := newTestScaler(newFakeClient(t, is), sc, ag)
		_, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()},
			MetricName:      "vllm:num_requests_waiting",
		})
		assert.Error(t, err)
		assert.Equal(t, 0, ag.callCount)
	})
}

func TestKaitoScaler_GetMetrics_Routing(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)

	serviceSource := &stubSource{snapshot: &metricsource.MetricSnapshot{}}
	eppSource := &stubSource{snapshot: &metricsource.MetricSnapshot{}}
	sumAgg := &stubAggregator{value: 1}
	serviceAvgAgg := &stubAggregator{value: 2}

	s := NewKaitoScaler(newFakeClient(t, is),
		map[string]metricsource.MetricSource{"service": serviceSource, "epp": eppSource},
		map[string]aggregator.Aggregator{"sum": sumAgg, "service-avg": serviceAvgAgg},
	)

	meta := newValidScalerMetadata()
	meta[constants.MetricSourceInMetadata] = "epp"
	meta[constants.AggregationInMetadata] = "service-avg"
	meta[constants.MetricNameInMetadata] = "inference_pool_per_pod_queue_size"

	resp, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
		ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: meta},
		MetricName:      "inference_pool_per_pod_queue_size",
	})
	assert.NoError(t, err)
	assert.Equal(t, float64(2), resp.MetricValues[0].MetricValueFloat)
	// Only the EPP source and service-average aggregator should have been used.
	assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "is1"}, eppSource.gotIS)
	assert.Equal(t, types.NamespacedName{}, serviceSource.gotIS)
	assert.Equal(t, 1, serviceAvgAgg.callCount)
	assert.Equal(t, 0, sumAgg.callCount)
}

func TestKaitoScaler_GetMetrics_Quantile(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)

	// A histogram with all 100 observations in the (1, 2] bucket, so any quantile
	// falls in that bucket and is interpolated between 1 and 2.
	snap := &metricsource.MetricSnapshot{
		Services: []metricsource.ServiceMetrics{{
			Name:      "epp0",
			Namespace: "ns1",
			Histograms: map[string]metricsource.Histogram{
				"inference_objective_request_duration_seconds": {
					Buckets: []metricsource.Bucket{
						{Le: 1, CumulativeCount: 0},
						{Le: 2, CumulativeCount: 100},
					},
					Count: 100,
				},
			},
		}},
	}

	newMeta := func(quantile string) map[string]string {
		meta := newValidScalerMetadata()
		meta[constants.MetricSourceInMetadata] = "epp"
		meta[constants.AggregationInMetadata] = aggregator.QuantileAggregatorName
		meta[constants.MetricNameInMetadata] = "inference_objective_request_duration_seconds"
		if quantile != "" {
			meta[constants.QuantileInMetadata] = quantile
		}
		return meta
	}

	newScaler := func() *KaitoScaler {
		return NewKaitoScaler(newFakeClient(t, is),
			map[string]metricsource.MetricSource{"epp": &stubSource{snapshot: snap}},
			map[string]aggregator.Aggregator{
				aggregator.QuantileAggregatorName: aggregator.NewQuantileAggregator(),
			},
		)
	}

	t.Run("default p95", func(t *testing.T) {
		resp, err := newScaler().GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: newMeta("")},
			MetricName:      "inference_objective_request_duration_seconds",
		})
		assert.NoError(t, err)
		assert.InDelta(t, 1.95, resp.MetricValues[0].MetricValueFloat, 1e-9)
	})

	t.Run("custom p50", func(t *testing.T) {
		resp, err := newScaler().GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: newMeta("0.5")},
			MetricName:      "inference_objective_request_duration_seconds",
		})
		assert.NoError(t, err)
		assert.InDelta(t, 1.5, resp.MetricValues[0].MetricValueFloat, 1e-9)
	})
}

func TestKaitoScaler_GetMetrics_UnknownSourceOrAggregation(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	s := newTestScaler(newFakeClient(t, is), &stubSource{snapshot: &metricsource.MetricSnapshot{}}, &stubAggregator{})

	t.Run("unknown source", func(t *testing.T) {
		meta := newValidScalerMetadata()
		meta[constants.MetricSourceInMetadata] = "nope"
		_, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: meta},
			MetricName:      "vllm:num_requests_waiting",
		})
		assert.Error(t, err)
	})

	t.Run("unknown aggregation", func(t *testing.T) {
		meta := newValidScalerMetadata()
		meta[constants.AggregationInMetadata] = "nope"
		_, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: meta},
			MetricName:      "vllm:num_requests_waiting",
		})
		assert.Error(t, err)
	})
}

func TestKaitoScaler_GetMetrics_Gate(t *testing.T) {
	tests := []struct {
		name          string
		replicas      int
		readyReplicas int
		want          float64
	}{
		{name: "not all ready -> gate not ready", replicas: 3, readyReplicas: 2, want: 0},
		{name: "all ready -> gate ready", replicas: 3, readyReplicas: 3, want: 1},
		{name: "no replicas -> gate ready", replicas: 0, readyReplicas: 0, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := &kaitov1beta1.InferenceSet{
				ObjectMeta: metav1.ObjectMeta{Name: "is1", Namespace: "ns1"},
				Spec: kaitov1beta1.InferenceSetSpec{
					Replicas: ptr.To(int32(tt.replicas)),
				},
				Status: kaitov1beta1.InferenceSetStatus{
					ReadyReplicas: tt.readyReplicas,
				},
			}
			// A metric source that would fail if called, to prove the gate path never scrapes.
			sc := &stubSource{err: errors.New("should not scrape")}
			ag := &stubAggregator{}
			s := NewKaitoScaler(newFakeClient(t, is),
				map[string]metricsource.MetricSource{"service": sc},
				map[string]aggregator.Aggregator{"sum": ag},
			)

			meta := newValidScalerMetadata()
			meta[constants.AggregationInMetadata] = constants.AggregationGate
			meta[constants.MetricNameInMetadata] = "kaito_gate"

			resp, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
				ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: meta},
				MetricName:      "kaito_gate",
			})
			assert.NoError(t, err)
			assert.Equal(t, tt.want, resp.MetricValues[0].MetricValueFloat)
			assert.Equal(t, 0, ag.callCount)
			assert.Equal(t, types.NamespacedName{}, sc.gotIS)
		})
	}
}

func TestReadinessGateValue(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int32
		ready    int
		want     float64
	}{
		{name: "nil replicas defaults to 1, none ready", ready: 0, want: 0},
		{name: "nil replicas defaults to 1, one ready", ready: 1, want: 1},
		{name: "partially ready", replicas: ptr.To[int32](3), ready: 2, want: 0},
		{name: "fully ready", replicas: ptr.To[int32](3), ready: 3, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := &kaitov1beta1.InferenceSet{}
			is.Spec.Replicas = tt.replicas
			is.Status.ReadyReplicas = tt.ready
			assert.Equal(t, tt.want, readinessGateValue(is))
		})
	}
}
