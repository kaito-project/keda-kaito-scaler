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

	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

func TestServiceAverageAggregator(t *testing.T) {
	agg := NewServiceAverageAggregator()
	const metric = "vllm:num_requests_waiting"

	t.Run("averages per-service scalars across services", func(t *testing.T) {
		// 2 services (replicas) with per-service summed queue depths 8 and 4 ->
		// per-replica average 6, independent of how many label sets produced them.
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Metrics: map[string]float64{metric: 8}},
				{Metrics: map[string]float64{metric: 4}},
			},
		}
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.NoError(t, err)
		assert.Equal(t, 6.0, got)
	})

	t.Run("denominator is service count, not series count", func(t *testing.T) {
		// Both services expose multi-label-set metrics already folded into
		// Metrics (8 and 4). The average must divide by 2 services, not by the
		// number of underlying label sets.
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Metrics: map[string]float64{metric: 8}},
				{Metrics: map[string]float64{metric: 4}},
			},
		}
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.NoError(t, err)
		assert.Equal(t, 6.0, got)
	})

	t.Run("skips failed services and averages successful ones", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Metrics: map[string]float64{metric: 10}},
				{Err: errors.New("scrape failed")},
			},
		}
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.NoError(t, err)
		assert.Equal(t, 10.0, got)
	})

	t.Run("errors when no service scraped successfully", func(t *testing.T) {
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Err: errors.New("scrape failed")},
			},
		}
		_, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.Error(t, err)
	})

	t.Run("metric present on some services averages with absent-as-zero", func(t *testing.T) {
		// Present on one service (value 8) and absent on another scraped service
		// (no traffic yet -> contributes 0): per-replica average is 8/2 = 4.
		// Absence on *some* services is a genuine 0.
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Metrics: map[string]float64{metric: 8}},
				{Metrics: map[string]float64{"other": 1}},
			},
		}
		got, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.NoError(t, err)
		assert.Equal(t, 4.0, got)
	})

	t.Run("errors when metric absent on all scraped services", func(t *testing.T) {
		// Every service scraped OK but none expose the metric: a misconfiguration,
		// not a real 0, so it must error (KEDA then holds instead of scaling down).
		snap := &metricsource.MetricSnapshot{
			Services: []metricsource.ServiceMetrics{
				{Metrics: map[string]float64{"other": 1}},
			},
		}
		_, err := agg.Aggregate(snap, AggregateInput{MetricName: metric})
		assert.Error(t, err)
	})

	t.Run("errors on nil snapshot", func(t *testing.T) {
		_, err := agg.Aggregate(nil, AggregateInput{MetricName: metric})
		assert.Error(t, err)
	})

	t.Run("errors when no services present", func(t *testing.T) {
		_, err := agg.Aggregate(&metricsource.MetricSnapshot{}, AggregateInput{MetricName: metric})
		assert.Error(t, err)
	})
}
