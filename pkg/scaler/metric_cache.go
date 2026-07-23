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
	"sync"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

const (
	// defaultCachePollInterval is how often the background poller re-scrapes each
	// tracked InferenceSet. It is fixed (not user-configurable) and matches the
	// default KEDA polling interval so the cache is at least as fresh as KEDA's
	// queries.
	defaultCachePollInterval = 15 * time.Second

	// defaultCacheStaleAfter bounds how old the newest cached snapshot may be
	// before the cache reports the target as unavailable. It is set to a few poll
	// intervals so a single slow or failed scrape does not immediately flip to a
	// "hold": only once scraping has been failing this long does GetMetrics return
	// an error, which makes KEDA/HPA hold the current replica count rather than
	// scale on stale data.
	defaultCacheStaleAfter = 3 * defaultCachePollInterval

	// defaultCacheTTL evicts a target once KEDA has not queried it for this long.
	// It is kept comfortably larger than the metric cache window so a brief gap in
	// KEDA polling does not discard an accumulated window. It is only a backstop
	// for a target whose InferenceSet still exists but is no longer referenced by
	// any ScaledObject trigger (e.g. a metric removed from the config): a *deleted*
	// InferenceSet is evicted immediately on the next poll, when the API returns
	// NotFound, so the TTL is never what cleans up a deletion.
	defaultCacheTTL = 20 * time.Minute
)

// MetricCache keeps an in-memory, per-InferenceSet rolling window of scraped
// metric snapshots so the scaler can serve every metric (gauge and histogram)
// from memory instead of scraping live on each KEDA request.
//
// Because keda-kaito-scaler runs as multiple replicas and KEDA may route
// successive GetMetrics calls to different replicas, an on-demand per-request
// window (built only on the replica that happens to serve a call) would be
// incomplete elsewhere. Instead every replica runs its own background poller
// that continuously scrapes each tracked InferenceSet into a full window, so any
// replica can answer any request from a complete local window.
//
// A target is registered lazily on the first GetMetrics that references it
// (ensure) and evicted when its InferenceSet is deleted or KEDA stops querying
// it for longer than the TTL.
type MetricCache struct {
	kubeClient client.Client
	sources    map[string]metricsource.MetricSource
	interval   time.Duration
	ttl        time.Duration
	staleAfter time.Duration

	// now is injectable so tests can advance time deterministically.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

// cacheEntry holds the rolling snapshot window for one scrape target.
type cacheEntry struct {
	is         types.NamespacedName
	scrapeCfg  metricsource.ScrapeConfig
	sourceName string
	// window is the largest cache window requested by any histogram metric on
	// this target; the snapshot history is pruned to it.
	window     time.Duration
	lastAccess time.Time
	samples    []snapshotSample
}

// snapshotSample is a scraped snapshot tagged with the time it was taken.
type snapshotSample struct {
	at   time.Time
	snap *metricsource.MetricSnapshot
}

// NewMetricCache builds a MetricCache over the given metric sources. The sources
// map must contain at least the "modelpod" source.
func NewMetricCache(kubeClient client.Client, sources map[string]metricsource.MetricSource) *MetricCache {
	return &MetricCache{
		kubeClient: kubeClient,
		sources:    sources,
		interval:   defaultCachePollInterval,
		ttl:        defaultCacheTTL,
		staleAfter: defaultCacheStaleAfter,
		now:        time.Now,
		entries:    make(map[string]*cacheEntry),
	}
}

// hasSource reports whether a metric source with the given name is registered.
func (c *MetricCache) hasSource(name string) bool {
	_, ok := c.sources[name]
	return ok
}

// entryKey identifies a scrape target by its InferenceSet, scrape endpoint, and
// source, so two triggers of the same InferenceSet that scrape a different
// endpoint or use a different source do not collide on one cache entry. The
// windowed-average aggregation rebuilds the same key from the AggregateInput
// (InferenceSet, ScrapeConfig, MetricSource) to locate its target. The scrape
// timeout is intentionally excluded: it does not change the target identity.
func entryKey(is types.NamespacedName, cfg metricsource.ScrapeConfig, source string) string {
	return fmt.Sprintf("%s/%s|%s|%s|%s|%s", is.Namespace, is.Name, cfg.Protocol, cfg.Port, cfg.Path, source)
}

// ensure registers a scrape target (idempotent) and records that it was just
// accessed. window is the histogram cache window requested by the calling
// trigger (0 for non-windowed metrics, which only need the newest snapshot); the
// entry retains history up to the largest window requested across its metrics.
// When the target is newly created, it is seeded with one synchronous scrape so
// the first request can already return data.
func (c *MetricCache) ensure(ctx context.Context, is types.NamespacedName, cfg metricsource.ScrapeConfig, source string, window time.Duration) {
	key := entryKey(is, cfg, source)

	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &cacheEntry{
			is:         is,
			scrapeCfg:  cfg,
			sourceName: source,
			window:     window,
			lastAccess: c.now(),
		}
		c.entries[key] = e
	} else {
		if window > e.window {
			e.window = window
		}
		e.lastAccess = c.now()
	}
	c.mu.Unlock()

	if !ok {
		// Seed the new entry synchronously (best-effort) so gauge/sum triggers
		// return data on their first request instead of erroring for one cycle.
		c.refresh(ctx, key)
	}
}

// Current registers the target (seeding it synchronously on first use) and
// returns its newest fresh snapshot for the current instant, or (nil, false)
// when the cache is cold (no snapshot yet) or stale (scraping has been failing).
// Returning false makes GetMetrics report the metric as unavailable so KEDA/HPA
// holds instead of scaling on missing or outdated data.
func (c *MetricCache) Current(ctx context.Context, is types.NamespacedName, cfg metricsource.ScrapeConfig, source string, window time.Duration) (*metricsource.MetricSnapshot, bool) {
	c.ensure(ctx, is, cfg, source, window)

	key := entryKey(is, cfg, source)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	// Cold start: no snapshot yet.
	if len(e.samples) == 0 {
		return nil, false
	}
	newest := e.samples[len(e.samples)-1]
	if c.now().Sub(newest.at) > c.staleAfter {
		// Scraping has been failing (or the target is cold); hold rather than
		// serve stale data.
		return nil, false
	}
	return newest.snap, true
}

// Name identifies the windowed-average aggregation served by the cache.
func (c *MetricCache) Name() string { return constants.AggregationWindowedAvg }

// Aggregate implements aggregator.Aggregator for the windowed-average
// aggregation: it returns the average observation of a histogram metric over
// input.Window, computed as (ΔSum / ΔCount) between the newest cached snapshot
// and the oldest snapshot still inside the window, for the target located via
// input.InferenceSet + input.ScrapeConfig + input.MetricSource. The passed
// snapshot is ignored (the cache holds the whole window).
//
// It returns an error — so GetMetrics reports the metric unavailable and KEDA/HPA
// holds — when the target has no snapshot yet, its newest snapshot is stale
// (scraping has been failing), or the cache has not yet accumulated a full
// window of history (warming up). Holding while warming up is deliberately
// conservative: GPU scale decisions are expensive, so we do not act on an
// average computed over a partial, possibly unrepresentative window. Once the
// window is filled it returns a value: 0 for an idle window (no new
// observations) or a cumulative reset from replica churn — a cold guard that
// permits scale-down without tripping scale-up.
func (c *MetricCache) Aggregate(_ *metricsource.MetricSnapshot, input aggregator.AggregateInput) (float64, error) {
	key := entryKey(input.InferenceSet, input.ScrapeConfig, input.MetricSource)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return 0, windowedUnavailableErr(input)
	}
	e.lastAccess = c.now()
	if len(e.samples) == 0 {
		return 0, windowedUnavailableErr(input)
	}

	newest := e.samples[len(e.samples)-1]
	if c.now().Sub(newest.at) > c.staleAfter {
		// Scraping has been failing; hold rather than average stale data.
		return 0, windowedUnavailableErr(input)
	}

	if len(e.samples) < 2 {
		// Not enough history yet to form a delta.
		return 0, windowedWarmingUpErr(input)
	}

	cutoff := newest.at.Add(-input.Window)
	// Baseline: the oldest sample still within the window.
	baseline := e.samples[0]
	for _, s := range e.samples {
		if !s.at.Before(cutoff) {
			baseline = s
			break
		}
	}

	// Coverage gate: hold until the retained samples span at least ~80% of the
	// requested window, so the average is representative of the whole window and
	// not a short recent slice (cold start, or scrape failures near the warm-up's
	// trailing edge). pruneSamples drops samples older than newest-window and caps
	// the retained span just under the window, so with any missed/jittered scrapes
	// the oldest retained sample can sit several poll intervals inside the window.
	// Requiring near-full coverage would then trip on ordinary scrape gaps and,
	// because one erroring trigger makes the whole composite metric unavailable,
	// needlessly freeze scaling; 80% tolerates that jitter while staying
	// representative.
	if newest.at.Sub(baseline.at) < input.Window*4/5 {
		return 0, windowedWarmingUpErr(input)
	}

	newSum, newCount, okNew := histogramSumCount(newest.snap, input.MetricName)
	baseSum, baseCount, okBase := histogramSumCount(baseline.snap, input.MetricName)
	if !okNew || !okBase {
		return 0, nil
	}
	dCount := newCount - baseCount
	if dCount <= 0 {
		return 0, nil
	}
	dSum := newSum - baseSum
	if dSum < 0 {
		return 0, nil
	}
	return dSum / dCount, nil
}

// windowedUnavailableErr is returned when a windowed metric cannot be served
// (the target has no snapshot yet or scraping has been failing), so KEDA/HPA
// holds instead of scaling on missing or stale data.
func windowedUnavailableErr(input aggregator.AggregateInput) error {
	return fmt.Errorf("windowed metric %q for inferenceset %s is not available (scrape failing or cold)", input.MetricName, input.InferenceSet)
}

// windowedWarmingUpErr is returned while the cache is still accumulating a full
// window of history, so KEDA/HPA holds rather than acting on a partial-window
// average.
func windowedWarmingUpErr(input aggregator.AggregateInput) error {
	return fmt.Errorf("windowed metric %q for inferenceset %s is warming up (cache window not yet filled)", input.MetricName, input.InferenceSet)
}

// histogramSumCount merges a histogram metric's _sum and _count across all
// successfully scraped services in the snapshot. ok is false when the metric is
// exposed by no scraped service.
func histogramSumCount(snap *metricsource.MetricSnapshot, metric string) (sum, count float64, ok bool) {
	if snap == nil {
		return 0, 0, false
	}
	for i := range snap.Services {
		svc := &snap.Services[i]
		if svc.Err != nil || svc.Histograms == nil {
			continue
		}
		h, found := svc.Histograms[metric]
		if !found {
			continue
		}
		ok = true
		sum += h.Sum
		count += float64(h.Count)
	}
	return sum, count, ok
}

// Start runs the background poll loop until ctx is cancelled. Every interval it
// re-scrapes each tracked target, evicting those whose InferenceSet is gone or
// that have not been queried within the TTL. It satisfies manager.Runnable.
func (c *MetricCache) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	klog.V(2).Infof("metric cache poller started (interval=%s ttl=%s)", c.interval, c.ttl)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

// poll refreshes every live target and evicts stale ones.
func (c *MetricCache) poll(ctx context.Context) {
	c.mu.Lock()
	keys := make([]string, 0, len(c.entries))
	for k, e := range c.entries {
		// Keep an idle target at least as long as its window, so a brief gap in
		// KEDA polling does not discard an accumulated window before it could be
		// reused.
		idleThreshold := c.ttl
		if e.window > idleThreshold {
			idleThreshold = e.window
		}
		if c.now().Sub(e.lastAccess) > idleThreshold {
			delete(c.entries, k)
			klog.V(4).Infof("evicted idle metric cache target %s", k)
			continue
		}
		keys = append(keys, k)
	}
	c.mu.Unlock()

	for _, k := range keys {
		c.refresh(ctx, k)
	}
}

// refresh scrapes the target once and appends the snapshot to its window,
// pruning samples older than the window. It evicts the target when its
// InferenceSet no longer exists.
func (c *MetricCache) refresh(ctx context.Context, key string) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return
	}

	is := &kaitov1beta1.InferenceSet{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Namespace: e.is.Namespace, Name: e.is.Name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			c.evict(key)
			klog.V(4).Infof("evicted metric cache target %s: InferenceSet deleted", key)
			return
		}
		klog.V(4).Infof("metric cache: failed to get InferenceSet for %s: %v", key, err)
		return
	}

	source := c.sources[e.sourceName]
	if source == nil {
		klog.Warningf("metric cache: unknown source %q for %s", e.sourceName, key)
		return
	}

	snap, err := source.Scrape(ctx, is, e.scrapeCfg)
	if err != nil {
		klog.V(4).Infof("metric cache: scrape failed for %s: %v", key, err)
		return
	}

	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.entries[key]
	if !ok {
		return
	}
	e.samples = pruneSamples(append(e.samples, snapshotSample{at: now, snap: snap}), now, e.window)
}

// evict removes a target from the cache.
func (c *MetricCache) evict(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// pruneSamples drops samples older than the window relative to now, always
// keeping at least the newest sample. It copies into a fresh slice so the pruned
// prefix can be garbage collected rather than lingering at the head of a growing
// backing array.
func pruneSamples(samples []snapshotSample, now time.Time, window time.Duration) []snapshotSample {
	if len(samples) <= 1 {
		return samples
	}
	cutoff := now.Add(-window)
	first := 0
	for first < len(samples)-1 && samples[first].at.Before(cutoff) {
		first++
	}
	if first == 0 {
		return samples
	}
	return append([]snapshotSample(nil), samples[first:]...)
}
