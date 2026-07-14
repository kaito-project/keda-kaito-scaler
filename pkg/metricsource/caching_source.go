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

package metricsource

import (
	"context"
	"fmt"
	"sync"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"k8s.io/klog/v2"
)

// CachingSource wraps another MetricSource with a short-lived, per-InferenceSet
// snapshot cache so that the several triggers of a composite ScaledObject do not
// each re-scrape every pod within the same KEDA polling cycle.
//
// The cache TTL is the per-call scrape timeout (ScrapeConfig.Timeout): the
// sequential GetMetrics calls KEDA issues for one HPA within a single cycle land
// within a few hundred milliseconds of each other (well under the timeout),
// while the next cycle (one PollingInterval later, which is always larger than
// the scrape timeout) always falls outside the TTL and forces a fresh scrape.
//
// Concurrent calls for the same key are coalesced by a per-key lock: only one
// underlying scrape runs at a time, and the others reuse its result. Only
// successful snapshots are cached; an error is returned to the caller without
// being stored so the next trigger retries.
//
// The cache is keyed by InferenceSet identity plus scrape endpoint. Because an
// InferenceSet maps to a long-lived LLM workload that is rarely created or
// deleted, the number of distinct keys stays bounded by the live workload count,
// so entries are not actively evicted.
type CachingSource struct {
	inner MetricSource

	mu      sync.Mutex
	entries map[string]cacheEntry
	keyLock map[string]*sync.Mutex

	// now is injectable so tests can advance time without sleeping; it defaults
	// to time.Now.
	now func() time.Time
}

type cacheEntry struct {
	snap *MetricSnapshot
	at   time.Time
}

// NewCachingSource returns a CachingSource that delegates to inner.
func NewCachingSource(inner MetricSource) *CachingSource {
	return &CachingSource{
		inner:   inner,
		entries: make(map[string]cacheEntry),
		keyLock: make(map[string]*sync.Mutex),
		now:     time.Now,
	}
}

// Name returns the name of the wrapped metric source.
func (c *CachingSource) Name() string {
	return c.inner.Name()
}

// Scrape returns a cached snapshot when a fresh one exists for the same
// InferenceSet and scrape configuration, otherwise it delegates to the wrapped
// metric source and caches the result. Caching is disabled when cfg.Timeout <= 0.
func (c *CachingSource) Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error) {
	ttl := cfg.Timeout
	key := cacheKey(is, cfg)

	// Fast path: a fresh snapshot is already cached.
	if snap, ok := c.freshSnapshot(key, ttl); ok {
		klog.V(6).Infof("reusing cached metrics snapshot for InferenceSet %s/%s", is.Namespace, is.Name)
		return snap, nil
	}

	// Slow path: serialize scrapes for this key so concurrent triggers coalesce
	// into a single underlying scrape.
	kl := c.keyMutex(key)
	kl.Lock()
	defer kl.Unlock()

	// Re-check under the per-key lock: a concurrent scrape may have populated the
	// cache while we waited.
	if snap, ok := c.freshSnapshot(key, ttl); ok {
		klog.V(6).Infof("reusing cached metrics snapshot for InferenceSet %s/%s", is.Namespace, is.Name)
		return snap, nil
	}

	snap, err := c.inner.Scrape(ctx, is, cfg)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[key] = cacheEntry{snap: snap, at: c.now()}
	c.mu.Unlock()
	return snap, nil
}

// freshSnapshot returns the cached snapshot for key when its age is below ttl.
func (c *CachingSource) freshSnapshot(key string, ttl time.Duration) (*MetricSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.at) >= ttl {
		return nil, false
	}
	return e.snap, true
}

// keyMutex returns the per-key mutex, creating it on first use.
func (c *CachingSource) keyMutex(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	kl, ok := c.keyLock[key]
	if !ok {
		kl = &sync.Mutex{}
		c.keyLock[key] = kl
	}
	return kl
}

// cacheKey identifies a snapshot by its InferenceSet and the scrape parameters
// that affect the result, so triggers using a different endpoint do not share a
// cached snapshot.
func cacheKey(is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) string {
	return fmt.Sprintf("%s/%s|%s|%s|%s", is.Namespace, is.Name, cfg.Protocol, cfg.Port, cfg.Path)
}
