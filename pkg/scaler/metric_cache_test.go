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
	"sync"
	"testing"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

const cacheTestMetric = "vllm:request_queue_time_seconds"

// seqSource returns a scripted sequence of snapshots on successive scrapes,
// repeating the last one once exhausted. It records the scrape config/IS seen.
type seqSource struct {
	mu     sync.Mutex
	snaps  []*metricsource.MetricSnapshot
	i      int
	gotIS  types.NamespacedName
	gotCfg metricsource.ScrapeConfig
	err    error
}

func (s *seqSource) Name() string { return metricsource.ModelPodSourceName }

func (s *seqSource) Scrape(_ context.Context, is *kaitov1beta1.InferenceSet, cfg metricsource.ScrapeConfig) (*metricsource.MetricSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotIS = types.NamespacedName{Namespace: is.Namespace, Name: is.Name}
	s.gotCfg = cfg
	if s.err != nil {
		return nil, s.err
	}
	snap := s.snaps[min(s.i, len(s.snaps)-1)]
	s.i++
	return snap, nil
}

func histSnapshot(sum float64, count uint64) *metricsource.MetricSnapshot {
	return &metricsource.MetricSnapshot{
		Services: []metricsource.ServiceMetrics{
			{Histograms: map[string]metricsource.Histogram{cacheTestMetric: {Sum: sum, Count: count}}},
		},
	}
}

// aggWindowed invokes the cache's windowed-average Aggregate for a target,
// mirroring how GetMetrics calls it.
func aggWindowed(c *MetricCache, nn types.NamespacedName, cfg metricsource.ScrapeConfig, metric string, window time.Duration) (float64, error) {
	return c.Aggregate(nil, aggregator.AggregateInput{
		InferenceSet: nn,
		ScrapeConfig: cfg,
		MetricSource: metricsource.ModelPodSourceName,
		MetricName:   metric,
		Window:       window,
	})
}

func TestMetricCache_WindowedAverage(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}
	window := 60 * time.Second

	src := &seqSource{snaps: []*metricsource.MetricSnapshot{
		histSnapshot(100, 50),
		histSnapshot(190, 80),
	}}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }

	// Seed at t0. The window is not yet filled, so Aggregate holds (error).
	cache.ensure(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window)
	_, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.Error(t, err)
	assert.Equal(t, nn, src.gotIS)
	assert.Equal(t, cfg, src.gotCfg)

	// A full window later a poll adds sample1 (190,80): the window is now filled,
	// so the average is computed: +90s over +30 requests -> avg 3s.
	now = base.Add(window)
	cache.poll(context.Background())
	v, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.NoError(t, err)
	assert.InDelta(t, 3.0, v, 1e-9)
}

func TestMetricCache_PrunesOldSamples(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}
	window := 60 * time.Second

	src := &seqSource{snaps: []*metricsource.MetricSnapshot{
		histSnapshot(0, 0),
		histSnapshot(300, 100),
		histSnapshot(600, 200),
	}}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }

	cache.ensure(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window) // t0 -> (0,0)
	now = base.Add(30 * time.Second)
	cache.poll(context.Background()) // t0+30 -> (300,100)
	now = base.Add(90 * time.Second)
	cache.poll(context.Background()) // t0+90 -> (600,200)

	// The base sample (90s old) falls out of the 60s window; baseline is t0+30:
	// delta 600-300=300 over 200-100=100 -> avg 3.
	v, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.NoError(t, err)
	assert.InDelta(t, 3.0, v, 1e-9)
}

func TestMetricCache_HoldsWhenStale(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}

	src := &seqSource{snaps: []*metricsource.MetricSnapshot{
		histSnapshot(100, 50),
		histSnapshot(190, 80),
	}}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }
	window := 60 * time.Second

	// Seed, then fill the window a full window later so both reads succeed.
	cache.ensure(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window)
	now = base.Add(window)
	cache.poll(context.Background())
	_, ok := cache.Current(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window)
	assert.True(t, ok)
	_, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.NoError(t, err)

	// Advance past staleAfter with no successful scrape: the newest snapshot goes
	// stale, so both reads report unavailable and GetMetrics holds.
	now = base.Add(window + defaultCacheStaleAfter + time.Second)
	_, ok = cache.Current(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window)
	assert.False(t, ok)
	_, err = aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.Error(t, err)
}

func TestMetricCache_Current(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}

	snap := &metricsource.MetricSnapshot{
		Services: []metricsource.ServiceMetrics{{Metrics: map[string]float64{"vllm:num_requests_waiting": 7}}},
	}
	src := &seqSource{snaps: []*metricsource.MetricSnapshot{snap}}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})

	// Current registers + seeds the target and returns the freshly scraped snapshot.
	got, ok := cache.Current(context.Background(), nn, cfg, metricsource.ModelPodSourceName, 0)
	assert.True(t, ok)
	assert.Same(t, snap, got)
}

func TestMetricCache_EvictsOnInferenceSetDeleted(t *testing.T) {
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "gone"}

	// Fake client has no InferenceSet, so Get returns NotFound during the seed
	// refresh and the target is evicted immediately.
	src := &seqSource{snaps: []*metricsource.MetricSnapshot{histSnapshot(1, 1)}}
	cache := NewMetricCache(newFakeClient(t), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})

	// Current seeds the target, but the seed scrape's Get returns NotFound, so the
	// target is evicted and Current reports unavailable.
	_, ok := cache.Current(context.Background(), nn, cfg, metricsource.ModelPodSourceName, 0)
	assert.False(t, ok, "target should have been evicted after InferenceSet NotFound")
}

func TestMetricCache_EvictsStaleTargets(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}

	src := &seqSource{snaps: []*metricsource.MetricSnapshot{histSnapshot(1, 1)}}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }

	_, ok := cache.Current(context.Background(), nn, cfg, metricsource.ModelPodSourceName, 0)
	assert.True(t, ok)

	// Advance well past the TTL without any access, then poll: the target is evicted.
	now = base.Add(defaultCacheTTL + time.Minute)
	cache.poll(context.Background())
	assert.Empty(t, cache.entries)
}
