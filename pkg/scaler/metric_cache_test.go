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
	"math/rand"
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

// steadySnapshots returns n cumulative histogram snapshots whose average
// observation between any two of them is always 3s.
func steadySnapshots(n int) []*metricsource.MetricSnapshot {
	snaps := make([]*metricsource.MetricSnapshot, n)
	for i := range snaps {
		snaps[i] = histSnapshot(float64(30*i), uint64(10*i))
	}
	return snaps
}

// TestMetricCache_WindowedAverage_SteadyState drives the real poll cadence
// (defaultCachePollInterval) and checks that, once warmed up, the windowed
// average is served on every query despite scrape-timestamp jitter, a missed
// scrape, or a window shorter than the poll interval.
func TestMetricCache_WindowedAverage_SteadyState(t *testing.T) {
	tests := []struct {
		name    string
		window  time.Duration
		jitter  time.Duration
		missAt  int // poll index whose scrape fails; -1 for none
		warmups int // polls after which every query must succeed
	}{
		{name: "60s window, no jitter", window: 60 * time.Second, missAt: -1, warmups: 4},
		{name: "60s window, 50ms jitter", window: 60 * time.Second, jitter: 50 * time.Millisecond, missAt: -1, warmups: 5},
		{name: "60s window, 2s jitter", window: 60 * time.Second, jitter: 2 * time.Second, missAt: -1, warmups: 5},
		{name: "60s window, 2s jitter, one missed scrape", window: 60 * time.Second, jitter: 2 * time.Second, missAt: 20, warmups: 5},
		{name: "10s window shorter than the poll interval", window: 10 * time.Second, jitter: 2 * time.Second, missAt: -1, warmups: 1},
		{name: "300s window, 2s jitter", window: 300 * time.Second, jitter: 2 * time.Second, missAt: -1, warmups: 21},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := newReadyInferenceSet("is1", "ns1", true)
			cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
			nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}
			src := &seqSource{snaps: steadySnapshots(200)}
			cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := base
			cache.now = func() time.Time { return now }
			rng := rand.New(rand.NewSource(1))

			cache.ensure(context.Background(), nn, cfg, metricsource.ModelPodSourceName, tt.window)
			for k := 1; k < 120; k++ {
				now = base.Add(time.Duration(k) * defaultCachePollInterval)
				if tt.jitter > 0 {
					now = now.Add(time.Duration(rng.Int63n(int64(tt.jitter))))
				}
				src.mu.Lock()
				src.err = nil
				if k == tt.missAt {
					src.err = errors.New("scrape failed")
				}
				src.mu.Unlock()
				cache.poll(context.Background())

				now = now.Add(time.Second)
				v, err := aggWindowed(cache, nn, cfg, cacheTestMetric, tt.window)
				if k < tt.warmups {
					continue
				}
				if assert.NoError(t, err, "poll %d", k) {
					assert.InDelta(t, 3.0, v, 1e-9, "poll %d", k)
				}
			}
		})
	}
}

// TestMetricCache_WindowedAverage_WarmsUpAgainAfterOutage checks that a
// baseline left over from before a long scrape outage is not used to stretch
// the average far beyond the window: the cache warms up again instead.
func TestMetricCache_WindowedAverage_WarmsUpAgainAfterOutage(t *testing.T) {
	is := newReadyInferenceSet("is1", "ns1", true)
	cfg := metricsource.ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics"}
	nn := types.NamespacedName{Namespace: "ns1", Name: "is1"}
	window := 60 * time.Second
	src := &seqSource{snaps: steadySnapshots(50)}
	cache := NewMetricCache(newFakeClient(t, is), map[string]metricsource.MetricSource{metricsource.ModelPodSourceName: src})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }
	pollAt := func(d time.Duration) {
		now = base.Add(d)
		cache.poll(context.Background())
	}

	cache.ensure(context.Background(), nn, cfg, metricsource.ModelPodSourceName, window)
	for d := 15 * time.Second; d <= 60*time.Second; d += 15 * time.Second {
		pollAt(d)
	}
	_, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.NoError(t, err)

	// Scraping fails from t=75s to t=185s, then recovers at t=200s.
	src.err = errors.New("scrape failed")
	for d := 75 * time.Second; d <= 185*time.Second; d += 15 * time.Second {
		pollAt(d)
	}
	src.err = nil
	pollAt(200 * time.Second)
	_, err = aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.ErrorContains(t, err, "warming up", "pre-outage baseline must not be used")

	for d := 215 * time.Second; d < 260*time.Second; d += 15 * time.Second {
		pollAt(d)
	}
	_, err = aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.ErrorContains(t, err, "warming up")

	pollAt(260 * time.Second)
	v, err := aggWindowed(cache, nn, cfg, cacheTestMetric, window)
	assert.NoError(t, err)
	assert.InDelta(t, 3.0, v, 1e-9)
}

func TestPruneSamples_KeepsBaselineAtWindowStart(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s float64) snapshotSample {
		return snapshotSample{at: base.Add(time.Duration(s * float64(time.Second)))}
	}
	window := 60 * time.Second
	tests := []struct {
		name    string
		samples []snapshotSample
		now     time.Time
		want    []snapshotSample
	}{
		{
			name:    "newest a few ms late keeps the sample just before the window start",
			samples: []snapshotSample{at(0), at(15), at(30), at(45), at(60.05)},
			now:     base.Add(60*time.Second + 50*time.Millisecond),
			want:    []snapshotSample{at(0), at(15), at(30), at(45), at(60.05)},
		},
		{
			name:    "drops samples older than the baseline",
			samples: []snapshotSample{at(-30), at(-15), at(0), at(15), at(30), at(45), at(60.05)},
			now:     base.Add(60*time.Second + 50*time.Millisecond),
			want:    []snapshotSample{at(0), at(15), at(30), at(45), at(60.05)},
		},
		{
			name:    "sample exactly at the window start is the baseline",
			samples: []snapshotSample{at(-15), at(0), at(15), at(60)},
			now:     base.Add(60 * time.Second),
			want:    []snapshotSample{at(0), at(15), at(60)},
		},
		{
			name:    "keeps the newest sample even when everything is old",
			samples: []snapshotSample{at(0), at(15)},
			now:     base.Add(10 * time.Minute),
			want:    []snapshotSample{at(15)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pruneSamples(tt.samples, tt.now, window))
		})
	}
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
