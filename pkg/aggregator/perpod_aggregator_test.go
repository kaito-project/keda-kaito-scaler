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

func TestPerPodAverageAggregator(t *testing.T) {
	agg := NewPerPodAverageAggregator()
	const metric = "inference_pool_per_pod_queue_size"

	t.Run("float average avoids integer truncation", func(t *testing.T) {
		// 2 pods with queue sizes 2 and 1 -> average 1.5 (EPP's own integer
		// division would truncate 3/2 to 1).
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Series: map[string][]float64{metric: {2, 1}}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 1.5, got)
	})

	t.Run("averages across multiple services", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Series: map[string][]float64{metric: {4}}},
				{Series: map[string][]float64{metric: {2, 0}}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 2.0, got) // (4+2+0)/3
	})

	t.Run("no series returns zero", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{Services: []scraper.ServiceMetrics{{}}}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})

	t.Run("skips errored services", func(t *testing.T) {
		snap := &scraper.MetricSnapshot{
			Services: []scraper.ServiceMetrics{
				{Err: errors.New("boom"), Series: map[string][]float64{metric: {99}}},
				{Series: map[string][]float64{metric: {3, 1}}},
			},
		}
		got, err := agg.Aggregate(snap, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 2.0, got) // only (3+1)/2, errored service ignored
	})

	t.Run("nil snapshot returns zero", func(t *testing.T) {
		got, err := agg.Aggregate(nil, metric, 0)
		assert.NoError(t, err)
		assert.Equal(t, 0.0, got)
	})
}
