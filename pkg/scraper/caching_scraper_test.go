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

package scraper

import (
	"context"
	"errors"
	"testing"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeScraper is a test double for Scraper that counts calls and records the
// InferenceSet it was asked to scrape.
type fakeScraper struct {
	callCount int
	err       error
	snapshot  *MetricSnapshot
}

func (f *fakeScraper) Scrape(_ context.Context, is *kaitov1beta1.InferenceSet, _ ScrapeConfig) (*MetricSnapshot, error) {
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	snap := f.snapshot
	if snap == nil {
		snap = &MetricSnapshot{}
	}
	return snap, nil
}

func newIS(namespace, name string) *kaitov1beta1.InferenceSet {
	return &kaitov1beta1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

func TestCachingScraper(t *testing.T) {
	ctx := context.Background()
	cfg := ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics", Timeout: 3 * time.Second}

	t.Run("reuses cached snapshot within TTL", func(t *testing.T) {
		inner := &fakeScraper{snapshot: &MetricSnapshot{}}
		c := NewCachingScraper(inner)
		is := newIS("ns1", "is1")

		// Two consecutive scrapes (e.g. two composite triggers) within the TTL
		// should trigger only a single underlying scrape.
		s1, err := c.Scrape(ctx, is, cfg)
		assert.NoError(t, err)
		s2, err := c.Scrape(ctx, is, cfg)
		assert.NoError(t, err)

		assert.Equal(t, 1, inner.callCount)
		assert.Same(t, s1, s2)
	})

	t.Run("re-scrapes after TTL expires", func(t *testing.T) {
		inner := &fakeScraper{snapshot: &MetricSnapshot{}}
		c := NewCachingScraper(inner)
		is := newIS("ns1", "is1")

		now := time.Unix(0, 0)
		c.now = func() time.Time { return now }

		_, err := c.Scrape(ctx, is, cfg)
		assert.NoError(t, err)
		assert.Equal(t, 1, inner.callCount)

		// Advance beyond the TTL (cfg.Timeout) -> next scrape hits the inner one.
		now = now.Add(cfg.Timeout + time.Millisecond)
		_, err = c.Scrape(ctx, is, cfg)
		assert.NoError(t, err)
		assert.Equal(t, 2, inner.callCount)
	})

	t.Run("different InferenceSets are cached separately", func(t *testing.T) {
		inner := &fakeScraper{snapshot: &MetricSnapshot{}}
		c := NewCachingScraper(inner)

		_, err := c.Scrape(ctx, newIS("ns1", "is1"), cfg)
		assert.NoError(t, err)
		_, err = c.Scrape(ctx, newIS("ns1", "is2"), cfg)
		assert.NoError(t, err)

		assert.Equal(t, 2, inner.callCount)
	})

	t.Run("different scrape endpoints are cached separately", func(t *testing.T) {
		inner := &fakeScraper{snapshot: &MetricSnapshot{}}
		c := NewCachingScraper(inner)
		is := newIS("ns1", "is1")

		_, err := c.Scrape(ctx, is, cfg)
		assert.NoError(t, err)
		other := cfg
		other.Port = "8080"
		_, err = c.Scrape(ctx, is, other)
		assert.NoError(t, err)

		assert.Equal(t, 2, inner.callCount)
	})

	t.Run("errors are not cached", func(t *testing.T) {
		inner := &fakeScraper{err: errors.New("scrape failed")}
		c := NewCachingScraper(inner)
		is := newIS("ns1", "is1")

		_, err := c.Scrape(ctx, is, cfg)
		assert.Error(t, err)
		_, err = c.Scrape(ctx, is, cfg)
		assert.Error(t, err)

		// No successful snapshot was cached, so both calls hit the inner scraper.
		assert.Equal(t, 2, inner.callCount)
	})

	t.Run("caching disabled when timeout is not positive", func(t *testing.T) {
		inner := &fakeScraper{snapshot: &MetricSnapshot{}}
		c := NewCachingScraper(inner)
		is := newIS("ns1", "is1")
		noTTL := cfg
		noTTL.Timeout = 0

		_, err := c.Scrape(ctx, is, noTTL)
		assert.NoError(t, err)
		_, err = c.Scrape(ctx, is, noTTL)
		assert.NoError(t, err)

		assert.Equal(t, 2, inner.callCount)
	})
}
