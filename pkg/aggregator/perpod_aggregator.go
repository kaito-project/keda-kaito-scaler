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

import "github.com/kaito-project/keda-kaito-scaler/pkg/scraper"

// PerPodAverageAggregator averages a per-series gauge across all series in a
// snapshot using floating-point division, avoiding the integer-division
// truncation that EPP's own inference_pool_average_queue_size suffers from.
//
// It reads scraper.ServiceMetrics.Series[metricName] (each entry is one backend pod's
// raw value, e.g. inference_pool_per_pod_queue_size) across every scraped
// service and returns sum(values) / count(values). When no series are present
// (no ready pods, or the metric is absent) it returns 0 so the scaler treats
// the signal as "no pressure" rather than erroring.
type PerPodAverageAggregator struct{}

// NewPerPodAverageAggregator constructs a PerPodAverageAggregator.
func NewPerPodAverageAggregator() *PerPodAverageAggregator {
	return &PerPodAverageAggregator{}
}

// Aggregate computes the floating-point average of metricName's per-series
// values across all services. The threshold argument is unused (kept to satisfy
// the Aggregator interface). Returns 0 when there are no series.
func (a *PerPodAverageAggregator) Aggregate(snapshot *scraper.MetricSnapshot, metricName string, _ float64) (float64, error) {
	if snapshot == nil {
		return 0, nil
	}

	var sum float64
	var count int
	for i := range snapshot.Services {
		svc := &snapshot.Services[i]
		if svc.Err != nil || svc.Series == nil {
			continue
		}
		for _, v := range svc.Series[metricName] {
			sum += v
			count++
		}
	}

	if count == 0 {
		return 0, nil
	}
	return sum / float64(count), nil
}
