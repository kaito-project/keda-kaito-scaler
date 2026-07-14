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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

func TestQuantileAggregator(t *testing.T) {
	const metric = "inference_objective_request_duration_seconds"
	agg := NewQuantileAggregator()

	t.Run("p95 linear interpolation matches Prometheus", func(t *testing.T) {
		// Mirrors a live EPP snapshot: 105 observations, p95 rank = 99.75 falls
		// between le=1.25 (cumulative 99) and le=1.5 (cumulative 105):
		//   1.25 + (1.5-1.25)*(99.75-99)/(105-99) = 1.28125
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Histograms: map[string]metricsource.Histogram{
					metric: {
						Buckets: []metricsource.Bucket{
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
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.NoError(t, err)
		assert.InDelta(t, 1.28125, got, 1e-9)
	})

	t.Run("merges histograms across services", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Histograms: map[string]metricsource.Histogram{metric: {
					Buckets: []metricsource.Bucket{
						{Le: 1.0, CumulativeCount: 45},
						{Le: 2.0, CumulativeCount: 50},
						{Le: math.Inf(1), CumulativeCount: 50},
					},
				}}},
				{Histograms: map[string]metricsource.Histogram{metric: {
					Buckets: []metricsource.Bucket{
						{Le: 1.0, CumulativeCount: 45},
						{Le: 2.0, CumulativeCount: 50},
						{Le: math.Inf(1), CumulativeCount: 50},
					},
				}}},
			},
		}
		// Merged: le=1.0->90, le=2.0->100, +Inf->100. total 100, rank 95.
		// 1.0 + (2.0-1.0)*(95-90)/(100-90) = 1.5
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.NoError(t, err)
		assert.InDelta(t, 1.5, got, 1e-9)
	})

	t.Run("errors when histogram absent on all scraped services", func(t *testing.T) {
		// Scraped OK but the histogram is exposed by no service: a
		// misconfiguration, not a real 0, so it must error (KEDA then holds).
		snap := &metricsource.MetricSnapshot{Services: []metricsource.ServiceMetrics{{}}}
		_, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.Error(t, err)
	})

	t.Run("all services errored returns error", func(t *testing.T) {
		// A total scrape outage must be reported as unknown (error), not as 0,
		// so it cannot masquerade as "no latency pressure" and trigger a
		// spurious scale-down.
		snap := &metricsource.MetricSnapshot{Services: []metricsource.ServiceMetrics{
			{Name: "a", Err: errors.New("scrape failed")},
			{Name: "b", Err: errors.New("scrape failed")},
		}}
		_, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.Error(t, err)
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		_, err := agg.Aggregate(nil, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.Error(t, err)
	})

	t.Run("empty observations returns zero", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{Services: []metricsource.ServiceMetrics{
			{Histograms: map[string]metricsource.Histogram{metric: {
				Buckets: []metricsource.Bucket{{Le: math.Inf(1), CumulativeCount: 0}},
			}}},
		}}
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 0.95})
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("quantile clamps out of range", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{Services: []metricsource.ServiceMetrics{
			{Histograms: map[string]metricsource.Histogram{metric: {
				Buckets: []metricsource.Bucket{
					{Le: 1, CumulativeCount: 0},
					{Le: 2, CumulativeCount: 100},
					{Le: math.Inf(1), CumulativeCount: 100},
				},
			}}},
		}}
		// Quantile > 1 is clamped to 1, matching an explicit p100 request.
		high, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 5})
		assert.NoError(t, err)
		atOne, err := agg.Aggregate(snap, AggregateInput{MetricName: metric, Quantile: 1})
		assert.NoError(t, err)
		assert.Equal(t, atOne, high)
	})
}
