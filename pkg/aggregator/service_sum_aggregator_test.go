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
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

func TestSumAggregator_Aggregate(t *testing.T) {
	agg := NewSumAggregator()

	tests := []struct {
		name       string
		snapshot   *metricsource.MetricSnapshot
		metricName string
		threshold  float64
		wantValue  float64
		wantErr    bool
	}{
		{
			name:     "nil snapshot errors",
			snapshot: nil,
			wantErr:  true,
		},
		{
			name: "empty services errors",
			snapshot: &metricsource.MetricSnapshot{
				InferenceSet: types.NamespacedName{Namespace: "ns", Name: "is"},
				Services:     nil,
			},
			wantErr: true,
		},
		{
			name: "sum over all successful services",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": 10}},
					{Name: "b", Metrics: map[string]float64{"m": 30}},
				},
			},
			metricName: "m",
			threshold:  5,
			wantValue:  40,
		},
		{
			name: "scale-down missing service compensated with threshold",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": 2}},
					{Name: "b", Err: errors.New("scrape failed")},
				},
			},
			metricName: "m",
			threshold:  10,
			// success avg=2 (< threshold 10 => scale-down). sum becomes 2+10=12.
			wantValue: 12,
		},
		{
			name: "scale-up missing service not compensated",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": 20}},
					{Name: "b", Err: errors.New("scrape failed")},
				},
			},
			metricName: "m",
			threshold:  10,
			// success avg=20 (>= threshold 10 => scale-up). sum stays 20.
			wantValue: 20,
		},
		{
			name: "metric name missing on a scraped service counts as zero",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": 4}},
					{Name: "b", Metrics: map[string]float64{"other": 99}},
				},
			},
			metricName: "m",
			threshold:  10,
			// Both services scraped OK; "b" lacks "m" so it contributes 0 and still
			// counts as a real replica. successCount=2=total => no compensation.
			wantValue: 4,
		},
		{
			name: "negative aggregated value is clamped to zero",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": -5}},
					{Name: "b", Metrics: map[string]float64{"m": -3}},
				},
			},
			metricName: "m",
			threshold:  10,
			wantValue:  0,
		},
		{
			name: "all services failed returns error",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Err: errors.New("x")},
					{Name: "b", Err: errors.New("y")},
				},
			},
			metricName: "m",
			threshold:  10,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := agg.Aggregate(tt.snapshot, AggregateInput{MetricName: tt.metricName, Threshold: tt.threshold})
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.InDelta(t, tt.wantValue, val, 1e-9)
		})
	}
}
