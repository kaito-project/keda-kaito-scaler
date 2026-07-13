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
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

// bkt is a compact (upper bound, cumulative count) pair for building test
// histogram buckets.
type bkt struct {
	le  float64
	cum uint64
}

func scalarFamily(name string, t dto.MetricType, values ...float64) *dto.MetricFamily {
	mf := &dto.MetricFamily{Name: ptr.To(name), Type: t.Enum()}
	for _, v := range values {
		m := &dto.Metric{}
		switch t {
		case dto.MetricType_COUNTER:
			m.Counter = &dto.Counter{Value: ptr.To(v)}
		case dto.MetricType_UNTYPED:
			m.Untyped = &dto.Untyped{Value: ptr.To(v)}
		default:
			m.Gauge = &dto.Gauge{Value: ptr.To(v)}
		}
		mf.Metric = append(mf.Metric, m)
	}
	return mf
}

func histMetric(count uint64, sum float64, buckets ...bkt) *dto.Metric {
	h := &dto.Histogram{SampleCount: ptr.To(count), SampleSum: ptr.To(sum)}
	for _, b := range buckets {
		h.Bucket = append(h.Bucket, &dto.Bucket{UpperBound: ptr.To(b.le), CumulativeCount: ptr.To(b.cum)})
	}
	return &dto.Metric{Histogram: h}
}

func histFamily(name string, metrics ...*dto.Metric) *dto.MetricFamily {
	return &dto.MetricFamily{Name: ptr.To(name), Type: dto.MetricType_HISTOGRAM.Enum(), Metric: metrics}
}

func TestParseFamilies(t *testing.T) {
	families := map[string]*dto.MetricFamily{
		// Gauge summed across its label sets.
		"g": scalarFamily("g", dto.MetricType_GAUGE, 2, 3),
		// Counter and untyped are summed too.
		"c": scalarFamily("c", dto.MetricType_COUNTER, 4),
		"u": scalarFamily("u", dto.MetricType_UNTYPED, 1.5),
		// A family with no metrics contributes nothing.
		"empty": scalarFamily("empty", dto.MetricType_GAUGE),
		// A histogram goes to Histograms, never to the scalar map.
		"h": histFamily("h", histMetric(10, 12.5, bkt{1, 4}, bkt{math.Inf(1), 10})),
	}

	metrics, histograms := parseFamilies(families)

	assert.Equal(t, 5.0, metrics["g"])
	assert.Equal(t, 4.0, metrics["c"])
	assert.Equal(t, 1.5, metrics["u"])

	_, ok := metrics["empty"]
	assert.False(t, ok, "empty family must not appear in scalar metrics")
	_, ok = metrics["h"]
	assert.False(t, ok, "histogram family must not appear in scalar metrics")

	h, ok := histograms["h"]
	assert.True(t, ok)
	assert.Equal(t, uint64(10), h.Count)
	assert.Equal(t, 12.5, h.Sum)
	assert.Equal(t, []Bucket{
		{Le: 1, CumulativeCount: 4},
		{Le: math.Inf(1), CumulativeCount: 10},
	}, h.Buckets)

	// The histogram family is not a scalar, so only "g"/"c"/"u" made it in.
	assert.Len(t, metrics, 3)
	assert.Len(t, histograms, 1)
}

func TestParseFamilies_ScalarWithoutValueSkipped(t *testing.T) {
	// A gauge-typed family whose single metric carries no gauge/counter/untyped
	// value yields no scalar (ExtractScalarValue returns false).
	families := map[string]*dto.MetricFamily{
		"noval": {
			Name:   ptr.To("noval"),
			Type:   dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{}},
		},
	}

	metrics, histograms := parseFamilies(families)

	assert.Empty(t, metrics)
	assert.Empty(t, histograms)
}

func TestMergeHistogram_SingleSeriesSorted(t *testing.T) {
	// Buckets provided out of order must come back sorted ascending by Le.
	mf := histFamily("d", histMetric(3, 6.0, bkt{2, 3}, bkt{1, 1}, bkt{math.Inf(1), 3}))

	h, ok := mergeHistogram(mf)
	assert.True(t, ok)
	assert.Equal(t, []Bucket{
		{Le: 1, CumulativeCount: 1},
		{Le: 2, CumulativeCount: 3},
		{Le: math.Inf(1), CumulativeCount: 3},
	}, h.Buckets)
	assert.Equal(t, uint64(3), h.Count)
	assert.Equal(t, 6.0, h.Sum)
}

func TestMergeHistogram_MergesAcrossSeries(t *testing.T) {
	// Two label sets of the same family: bucket counts sum per upper bound and
	// the sums add up.
	mf := histFamily("d",
		histMetric(50, 20.0, bkt{1, 45}, bkt{2, 50}, bkt{math.Inf(1), 50}),
		histMetric(50, 30.0, bkt{1, 45}, bkt{2, 50}, bkt{math.Inf(1), 50}),
	)

	h, ok := mergeHistogram(mf)
	assert.True(t, ok)
	assert.Equal(t, []Bucket{
		{Le: 1, CumulativeCount: 90},
		{Le: 2, CumulativeCount: 100},
		{Le: math.Inf(1), CumulativeCount: 100},
	}, h.Buckets)
	// Count comes from the merged +Inf bucket, Sum is the total.
	assert.Equal(t, uint64(100), h.Count)
	assert.Equal(t, 50.0, h.Sum)
}

func TestMergeHistogram_InfBucketOverridesSampleCount(t *testing.T) {
	// When a +Inf bucket is present its count wins over SampleCount so quantile
	// interpolation stays consistent with the bucket ladder.
	mf := histFamily("d", histMetric(999, 1.0, bkt{1, 5}, bkt{math.Inf(1), 8}))

	h, ok := mergeHistogram(mf)
	assert.True(t, ok)
	assert.Equal(t, uint64(8), h.Count)
}

func TestMergeHistogram_NoInfBucketUsesSampleCount(t *testing.T) {
	// Without a +Inf bucket the summed SampleCount is used.
	mf := histFamily("d", histMetric(7, 1.0, bkt{1, 3}, bkt{2, 7}))

	h, ok := mergeHistogram(mf)
	assert.True(t, ok)
	assert.Equal(t, uint64(7), h.Count)
}

func TestMergeHistogram_NoHistogramSeries(t *testing.T) {
	// A histogram-typed family whose metrics carry no Histogram payload yields
	// nothing.
	mf := &dto.MetricFamily{
		Name:   ptr.To("d"),
		Type:   dto.MetricType_HISTOGRAM.Enum(),
		Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: ptr.To(1.0)}}},
	}

	_, ok := mergeHistogram(mf)
	assert.False(t, ok)
}

func TestMergeHistogram_EmptyFamily(t *testing.T) {
	_, ok := mergeHistogram(histFamily("d"))
	assert.False(t, ok)
}
