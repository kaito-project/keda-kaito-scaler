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
	"sort"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scraper"
)

// QuantileAggregator estimates a quantile (e.g. p95) from a Prometheus-style
// cumulative histogram, merging the histogram buckets of metricName across all
// scraped services and applying the same linear-interpolation formula as
// Prometheus' histogram_quantile.
type QuantileAggregator struct {
	// quantile is the target rank in [0, 1], e.g. 0.95 for p95.
	quantile float64
}

// NewQuantileAggregator constructs a QuantileAggregator for the given quantile
// in [0, 1]. Values outside the range are clamped.
func NewQuantileAggregator(quantile float64) *QuantileAggregator {
	if quantile < 0 {
		quantile = 0
	} else if quantile > 1 {
		quantile = 1
	}
	return &QuantileAggregator{quantile: quantile}
}

// Aggregate merges metricName's histogram buckets across all services and
// returns the interpolated quantile. The threshold argument is unused (kept to
// satisfy the Aggregator interface). Returns 0 when no histogram data or no
// observations are available.
func (a *QuantileAggregator) Aggregate(snapshot *scraper.MetricSnapshot, metricName string, _ float64) (float64, error) {
	if snapshot == nil {
		return 0, nil
	}

	// Merge cumulative bucket counts across services by upper bound.
	bucketCounts := make(map[float64]uint64)
	var totalCount uint64
	found := false
	for i := range snapshot.Services {
		svc := &snapshot.Services[i]
		if svc.Err != nil || svc.Histograms == nil {
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

	if !found || len(bucketCounts) == 0 {
		return 0, nil
	}

	buckets := make([]scraper.Bucket, 0, len(bucketCounts))
	for le, c := range bucketCounts {
		buckets = append(buckets, scraper.Bucket{Le: le, CumulativeCount: c})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Le < buckets[j].Le })

	// The total number of observations is the count in the highest (+Inf) bucket.
	totalCount = buckets[len(buckets)-1].CumulativeCount
	if totalCount == 0 {
		return 0, nil
	}

	return histogramQuantile(a.quantile, buckets, totalCount), nil
}

// histogramQuantile replicates Prometheus' histogram_quantile linear
// interpolation over cumulative buckets sorted ascending by upper bound.
func histogramQuantile(q float64, buckets []scraper.Bucket, totalCount uint64) float64 {
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
func finiteUpperBound(buckets []scraper.Bucket) float64 {
	for i := len(buckets) - 1; i >= 0; i-- {
		if !math.IsInf(buckets[i].Le, 1) {
			return buckets[i].Le
		}
	}
	return 0
}
