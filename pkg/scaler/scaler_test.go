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

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metrics"
)

// stubScraper is a test double for metrics.Scraper.
type stubScraper struct {
	snapshot *metrics.MetricSnapshot
	err      error
	gotCfg   metrics.ScrapeConfig
	gotIS    types.NamespacedName
}

func (s *stubScraper) Scrape(_ context.Context, is *kaitov1alpha1.InferenceSet, cfg metrics.ScrapeConfig) (*metrics.MetricSnapshot, error) {
	s.gotCfg = cfg
	s.gotIS = types.NamespacedName{Namespace: is.Namespace, Name: is.Name}
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

// stubAggregator is a test double for metrics.Aggregator.
type stubAggregator struct {
	value     float64
	err       error
	gotMetric string
	gotSnap   *metrics.MetricSnapshot
	threshold int64
	callCount int
}

func (a *stubAggregator) Aggregate(snap *metrics.MetricSnapshot, metricName string, threshold int64) (float64, error) {
	a.callCount++
	a.gotSnap = snap
	a.gotMetric = metricName
	a.threshold = threshold
	return a.value, a.err
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newValidScalerMetadata() map[string]string {
	return map[string]string{
		ScalerNameKeyInMetadata:         ScalerName,
		InferenceSetNameInMetadata:      "is1",
		InferenceSetNamespaceInMetadata: "ns1",
		MetricNameInMetadata:            "vllm:num_requests_waiting",
		MetricProtocolInMetadata:        "http",
		MetricPortInMetadata:            "80",
		MetricPathInMetadata:            "/metrics",
		ScrapeTimeoutInMetadata:         "3s",
		ThresholdInMetadata:             "10",
	}
}

func newReadyInferenceSet(name, namespace string, ready bool) *kaitov1alpha1.InferenceSet {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &kaitov1alpha1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: kaitov1alpha1.InferenceSetStatus{
			Conditions: []metav1.Condition{{
				Type:   string(kaitov1alpha1.InferenceSetConditionTypeReady),
				Status: status,
			}},
		},
	}
}

func TestNewKaitoScaler(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	sc := &stubScraper{}
	ag := &stubAggregator{}

	s := NewKaitoScaler(c, sc, ag)

	assert.NotNil(t, s)
	assert.Equal(t, c, s.kubeClient)
	assert.Equal(t, metrics.Scraper(sc), s.scraper)
	assert.Equal(t, metrics.Aggregator(ag), s.aggregator)
}

func TestParseScalerMetadata(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(m map[string]string)
		metricName string
		wantErr    bool
	}{
		{name: "valid"},
		{name: "missing scaler name", mutate: func(m map[string]string) { delete(m, ScalerNameKeyInMetadata) }, wantErr: true},
		{name: "wrong scaler name", mutate: func(m map[string]string) { m[ScalerNameKeyInMetadata] = "other" }, wantErr: true},
		{name: "missing inference set name", mutate: func(m map[string]string) { delete(m, InferenceSetNameInMetadata) }, wantErr: true},
		{name: "missing inference set namespace", mutate: func(m map[string]string) { delete(m, InferenceSetNamespaceInMetadata) }, wantErr: true},
		{name: "missing metric name", mutate: func(m map[string]string) { delete(m, MetricNameInMetadata) }, wantErr: true},
		{name: "invalid protocol", mutate: func(m map[string]string) { m[MetricProtocolInMetadata] = "ftp" }, wantErr: true},
		{name: "invalid timeout", mutate: func(m map[string]string) { m[ScrapeTimeoutInMetadata] = "abc" }, wantErr: true},
		{name: "invalid threshold", mutate: func(m map[string]string) { m[ThresholdInMetadata] = "abc" }, wantErr: true},
		{name: "metric name override", mutate: func(m map[string]string) { delete(m, MetricNameInMetadata) }, metricName: "override:metric"},
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
			assert.Equal(t, int64(10), cfg.Threshold)
			assert.Equal(t, 3*time.Second, cfg.ScrapeTimeout)
			if tt.metricName != "" {
				assert.Equal(t, tt.metricName, cfg.MetricName)
			}
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
			s := NewKaitoScaler(newFakeClient(t, objs...), &stubScraper{}, &stubAggregator{})

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
	s := NewKaitoScaler(newFakeClient(t), &stubScraper{}, &stubAggregator{})
	sor := &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()}
	resp, err := s.GetMetricSpec(context.Background(), sor)
	assert.NoError(t, err)
	assert.Len(t, resp.MetricSpecs, 1)
	assert.Equal(t, "vllm:num_requests_waiting", resp.MetricSpecs[0].MetricName)
	assert.Equal(t, int64(10), resp.MetricSpecs[0].TargetSize)
}

func TestKaitoScaler_GetMetrics(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)

	t.Run("delegates to scraper and aggregator", func(t *testing.T) {
		snap := &metrics.MetricSnapshot{
			InferenceSet: types.NamespacedName{Namespace: "ns1", Name: "is1"},
			Services: []metrics.ServiceMetrics{
				{Name: "ws0", Namespace: "ns1", Metrics: map[string]float64{"vllm:num_requests_waiting": 7}},
			},
		}
		sc := &stubScraper{snapshot: snap}
		ag := &stubAggregator{value: 7}
		s := NewKaitoScaler(newFakeClient(t, is), sc, ag)

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
		assert.Equal(t, int64(10), ag.threshold)
		assert.Same(t, snap, ag.gotSnap)
		assert.Equal(t, 1, ag.callCount)
	})

	t.Run("scraper failure bubbles up", func(t *testing.T) {
		sc := &stubScraper{err: errors.New("boom")}
		ag := &stubAggregator{}
		s := NewKaitoScaler(newFakeClient(t, is), sc, ag)
		_, err := s.GetMetrics(context.Background(), &externalscaler.GetMetricsRequest{
			ScaledObjectRef: &externalscaler.ScaledObjectRef{ScalerMetadata: newValidScalerMetadata()},
			MetricName:      "vllm:num_requests_waiting",
		})
		assert.Error(t, err)
		assert.Equal(t, 0, ag.callCount)
	})
}
