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
	"fmt"
	"math"
	"sort"

	"go.uber.org/multierr"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

// QuantileAggregator estimates a quantile (e.g. p95) from a Prometheus-style
// cumulative histogram, merging the histogram buckets of the metric across all
// scraped services and applying the same linear-interpolation formula as
// Prometheus' histogram_quantile. The target quantile is supplied per request
// via AggregateInput.Quantile, so a single instance serves every quantile.
type QuantileAggregator struct{}

// NewQuantileAggregator constructs a QuantileAggregator.
func NewQuantileAggregator() *QuantileAggregator {
	return &QuantileAggregator{}
}

// QuantileAggregatorName is the registered name of the QuantileAggregator.
const QuantileAggregatorName = "quantile"

// Name identifies the QuantileAggregator.
func (a *QuantileAggregator) Name() string { return QuantileAggregatorName }

// Aggregate merges the metric's histogram buckets across all services and
// returns the interpolated quantile (input.Quantile). Returns 0 when the
// histogram is present but has no observations yet (cold start / no traffic).
// Returns an error on a nil snapshot, when every service failed to scrape, or
// when the histogram is exposed by no scraped service (misconfigured metric
// name) — reporting a total outage or a missing metric as unknown rather than as
// "no latency pressure", which could otherwise trigger a spurious scale-down.
func (a *QuantileAggregator) Aggregate(snapshot *metricsource.MetricSnapshot, input AggregateInput) (float64, error) {
	metricName := input.MetricName
	// Clamp the target quantile to [0, 1]; callers should already validate it.
	quantile := input.Quantile
	if quantile < 0 {
		quantile = 0
	} else if quantile > 1 {
		quantile = 1
	}
	if snapshot == nil {
		return 0, fmt.Errorf("metric snapshot is nil")
	}

	// Merge cumulative bucket counts across services by upper bound.
	bucketCounts := make(map[float64]uint64)
	var totalCount uint64
	var (
		found        bool
		successCount int
		errs         []error
	)
	for i := range snapshot.Services {
		svc := &snapshot.Services[i]
		if svc.Err != nil {
			errs = append(errs, fmt.Errorf("service %s/%s: %w", svc.Namespace, svc.Name, svc.Err))
			continue
		}
		successCount++
		if svc.Histograms == nil {
			continue
		}
		h, ok := svc.Histograms[metricName]
		if !ok {
			continue
		}
		found = true
		for _, b := range h.Buckets {
			bucketCounts[b.Le] += b.CumulativeCount
		}
	}

	// Every service that exists failed to scrape: this is unknown, not zero.
	if len(snapshot.Services) > 0 && successCount == 0 {
		if combined := multierr.Combine(errs...); combined != nil {
			return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s: %w", metricName, snapshot.InferenceSet, combined)
		}
		return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s", metricName, snapshot.InferenceSet)
	}

	// The histogram was absent on every service that scraped successfully: treat
	// it as unknown (misconfigured metric name, or a metric the pods do not
	// expose) so KEDA holds rather than scaling down on a phantom 0.
	if !found {
		return 0, fmt.Errorf("metric %q not exposed by any scraped service for inferenceset %s", metricName, snapshot.InferenceSet)
	}
	// Found but no observations yet (cold start / no traffic): a genuine 0.
	if len(bucketCounts) == 0 {
		return 0, nil
	}

	buckets := make([]metricsource.Bucket, 0, len(bucketCounts))
	for le, c := range bucketCounts {
		buckets = append(buckets, metricsource.Bucket{Le: le, CumulativeCount: c})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Le < buckets[j].Le })

	// The total number of observations is the count in the highest (+Inf) bucket.
	totalCount = buckets[len(buckets)-1].CumulativeCount
	if totalCount == 0 {
		return 0, nil
	}

	return histogramQuantile(quantile, buckets, totalCount), nil
}

// histogramQuantile replicates Prometheus' histogram_quantile linear
// interpolation over cumulative buckets sorted ascending by upper bound.
func histogramQuantile(q float64, buckets []metricsource.Bucket, totalCount uint64) float64 {
	rank := q * float64(totalCount)

	// Find the first bucket whose cumulative count is >= rank.
	b := sort.Search(len(buckets), func(i int) bool {
		return float64(buckets[i].CumulativeCount) >= rank
	})
	if b == len(buckets) {
		// rank beyond the last finite bucket; return the highest finite bound.
		return finiteUpperBound(buckets)
	}

	upper := buckets[b].Le
	if math.IsInf(upper, 1) {
		return finiteUpperBound(buckets)
	}

	var lower float64
	var lowerCount float64
	if b > 0 {
		lower = buckets[b-1].Le
		lowerCount = float64(buckets[b-1].CumulativeCount)
	}
	upperCount := float64(buckets[b].CumulativeCount)

	if upperCount == lowerCount {
		return upper
	}
	return lower + (upper-lower)*(rank-lowerCount)/(upperCount-lowerCount)
}

// finiteUpperBound returns the largest non-+Inf bucket upper bound, or 0 when
// all buckets are +Inf.
func finiteUpperBound(buckets []metricsource.Bucket) float64 {
	for i := len(buckets) - 1; i >= 0; i-- {
		if !math.IsInf(buckets[i].Le, 1) {
			return buckets[i].Le
		}
	}
	return 0
}
