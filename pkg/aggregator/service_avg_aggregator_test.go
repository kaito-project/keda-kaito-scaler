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

package aggregator

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scraper"
)

func TestServiceAverageAggregator(t *testing.T) {
	agg := NewServiceAverageAggregator()
	const metric = "vllm:num_requests_waiting"

	t.Run("averages per-service scalars across services", func(t *testing.T) {
		// 2 services (replicas) with per-service summed queue depths 8 and 4 ->
		// per-replica average 6, independent of how many label sets produced them.
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Metrics: map[string]float64{metric: 8}},
				{Metrics: map[string]float64{metric: 4}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 6.0, got)
	})

	t.Run("denominator is service count, not series count", func(t *testing.T) {
		// Both services expose multi-label-set metrics already folded into
		// Metrics (8 and 4). The average must divide by 2 services, not by the
		// number of underlying label sets.
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Metrics: map[string]float64{metric: 8}},
				{Metrics: map[string]float64{metric: 4}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 6.0, got)
	})

	t.Run("skips failed services and averages successful ones", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Metrics: map[string]float64{metric: 10}},
				{Err: errors.New("scrape failed")},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 10.0, got)
	})

	t.Run("errors when no service scraped successfully", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Err: errors.New("scrape failed")},
			},
		}
		_, err := agg.Aggregate(snap, metric, 0)
		assert.Error(t, err)
	})

	t.Run("metric absent on scraped services counts as zero", func(t *testing.T) {
		// All services scraped OK but none expose the metric (cold start / no
		// traffic): each contributes 0, so the per-replica average is 0 rather
		// than an error that would block the composite formula.
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Metrics: map[string]float64{"other": 1}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("errors on nil snapshot", func(t *testing.T) {
		_, err := agg.Aggregate(nil, metric, 0)
		assert.Error(t, err)
	})

	t.Run("errors when no services present", func(t *testing.T) {
		_, err := agg.Aggregate(&scraper.MetricSnapshot{}, metric, 0)
		assert.Error(t, err)
	})
}
