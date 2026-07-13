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
	"math"
	"sort"

	dto "github.com/prometheus/client_model/go"

	"github.com/kaito-project/keda-kaito-scaler/pkg/util/promscrape"
)

// parseFamilies converts parsed Prometheus metric families into the two
// representations a ServiceMetrics carries: summed scalars (Metrics) and merged
// cumulative histograms (Histograms). Histogram families are folded into a
// single cumulative histogram per family; every other family type is summed
// across its label sets.
func parseFamilies(families map[string]*dto.MetricFamily) (map[string]float64, map[string]Histogram) {
	metricsMap := make(map[string]float64)
	histograms := make(map[string]Histogram)

	for familyName, mf := range families {
		if mf.GetType() == dto.MetricType_HISTOGRAM {
			if h, ok := mergeHistogram(mf); ok {
				histograms[familyName] = h
			}
			continue
		}

		var (
			sum   float64
			found bool
		)
		for _, m := range mf.GetMetric() {
			v, ok := promscrape.ExtractScalarValue(m)
			if !ok {
				continue
			}
			sum += v
			found = true
		}
		if found {
			metricsMap[familyName] = sum
		}
	}

	return metricsMap, histograms
}

// mergeHistogram merges all label-set series of a histogram family into a
// single cumulative histogram (bucket counts summed per upper bound, plus total
// count and sum). This lets a quantile aggregator operate on one endpoint that
// exposes a histogram split across multiple label sets.
func mergeHistogram(mf *dto.MetricFamily) (Histogram, bool) {
	bucketCounts := make(map[float64]uint64)
	var totalCount uint64
	var totalSum float64
	found := false

	for _, m := range mf.GetMetric() {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		found = true
		totalCount += h.GetSampleCount()
		totalSum += h.GetSampleSum()
		for _, b := range h.GetBucket() {
			bucketCounts[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}
	if !found {
		return Histogram{}, false
	}

	buckets := make([]Bucket, 0, len(bucketCounts))
	for le, c := range bucketCounts {
		buckets = append(buckets, Bucket{Le: le, CumulativeCount: c})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Le < buckets[j].Le })

	// If multiple series were summed, the +Inf bucket may not equal totalCount
	// due to per-series merging; prefer the +Inf bucket count when present so
	// quantile interpolation is consistent with the bucket ladder.
	if len(buckets) > 0 {
		last := buckets[len(buckets)-1]
		if math.IsInf(last.Le, 1) {
			totalCount = last.CumulativeCount
		}
	}

	return Histogram{Buckets: buckets, Count: totalCount, Sum: totalSum}, true
}
