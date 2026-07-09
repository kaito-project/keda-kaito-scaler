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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scraper"
)

func TestQuantileAggregator(t *testing.T) {
	const metric = "inference_objective_request_duration_seconds"
	agg := NewQuantileAggregator(0.95)

	t.Run("p95 linear interpolation matches Prometheus", func(t *testing.T) {
		// Mirrors a live EPP snapshot: 105 observations, p95 rank = 99.75 falls
		// between le=1.25 (cumulative 99) and le=1.5 (cumulative 105):
		//   1.25 + (1.5-1.25)*(99.75-99)/(105-99) = 1.28125
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Histograms: map[string]scraper.Histogram{
					metric: {
						Buckets: []scraper.Bucket{
							{Le: 1.0, CumulativeCount: 90},
							{Le: 1.25, CumulativeCount: 99},
							{Le: 1.5, CumulativeCount: 105},
							{Le: math.Inf(1), CumulativeCount: 105},
						},
						Count: 105,
						Sum:   62.45,
					},
				}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.InDelta(t, 1.28125, got, 1e-9)
	})

	t.Run("merges histograms across services", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Histograms: map[string]scraper.Histogram{metric: {
					Buckets: []scraper.Bucket{
						{Le: 1.0, CumulativeCount: 45},
						{Le: 2.0, CumulativeCount: 50},
						{Le: math.Inf(1), CumulativeCount: 50},
					},
				}}},
				{Histograms: map[string]scraper.Histogram{metric: {
					Buckets: []scraper.Bucket{
						{Le: 1.0, CumulativeCount: 45},
						{Le: 2.0, CumulativeCount: 50},
						{Le: math.Inf(1), CumulativeCount: 50},
					},
				}}},
			},
		}
		// Merged: le=1.0->90, le=2.0->100, +Inf->100. total 100, rank 95.
		// 1.0 + (2.0-1.0)*(95-90)/(100-90) = 1.5
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.InDelta(t, 1.5, got, 1e-9)
	})

	t.Run("missing histogram returns zero", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{Services: []scraper.ServiceMetrics{{}}}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("nil snapshot returns zero", func(t *testing.T) {
		got, err := agg.Aggregate(nil, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("empty observations returns zero", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{Services: []scraper.ServiceMetrics{
			{Histograms: map[string]scraper.Histogram{metric: {
				Buckets: []scraper.Bucket{{Le: math.Inf(1), CumulativeCount: 0}},
			}}},
		}}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("quantile clamps out of range", func(t *testing.T) {
		assert.Equal(t, 1.0, NewQuantileAggregator(5).quantile)
		assert.Equal(t, 0.0, NewQuantileAggregator(-1).quantile)
	})
}
