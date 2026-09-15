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

func TestServiceSumAggregator_Name(t *testing.T) {
	assert.Equal(t, ServiceSumAggregatorName, NewServiceSumAggregator().Name())
}

func TestServiceSumAggregator_Aggregate(t *testing.T) {
	agg := NewServiceSumAggregator()

	tests := []struct {
		name         string
		snapshot     *metricsource.MetricSnapshot
		metricName   string
		metricSource string
		threshold    float64
		wantValue    float64
		wantErr      bool
		wantErrMsg   string
	}{
		{
			name:     "nil snapshot errors",
			snapshot: nil,
			wantErr:  true,
		},
		{
			name: "missing EPP reports expected selector",
			snapshot: &metricsource.MetricSnapshot{
				InferenceSet: types.NamespacedName{Namespace: "ns", Name: "is"},
			},
			metricSource: metricsource.EPPSourceName,
			wantErr:      true,
			wantErrMsg:   "no ready Endpoint Picker pods available for selector llm-d-router-gateway=is-inferencepool-epp in namespace ns; this is expected temporarily during startup while KAITO creates the first Workspace and Endpoint Picker; if it persists, verify the InferenceSet uses a vLLM preset and the Gateway API Inference Extension is enabled",
		},
		{
			name: "sums across services",
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
			// Unlike the compensating sum aggregator, a service that could not
			// be scraped must not be imputed from the threshold: the EPP set is
			// the source of truth for demand, and inventing load for a missing
			// replica would wake or hold a workload that has no work queued.
			name: "failed service is skipped, not imputed",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": 3}},
					{Name: "b", Err: errors.New("connection refused")},
				},
			},
			metricName: "m",
			threshold:  100,
			wantValue:  3,
		},
		{
			// A Prometheus GaugeVec omits its series entirely until the first
			// observation, so an idle EPP reports success with no sample. That
			// is genuinely zero demand and must not be treated as an error, or
			// a parked workload would poll a permanent TriggerError.
			name: "scraped but metric absent counts as zero",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"other": 7}},
					{Name: "b", Metrics: map[string]float64{}},
				},
			},
			metricName: "m",
			threshold:  5,
			wantValue:  0,
		},
		{
			// Nothing was scraped at all, which is an observability failure
			// rather than an observation of zero, so it must surface as an error
			// and let KEDA hold the current replica count.
			name: "all services failed errors",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Err: errors.New("boom")},
					{Name: "b", Err: errors.New("boom")},
				},
			},
			metricName: "m",
			wantErr:    true,
		},
		{
			// Negative demand is meaningless and would corrupt the comparison
			// against the activation threshold.
			name: "negative total is clamped to zero",
			snapshot: &metricsource.MetricSnapshot{
				Services: []metricsource.ServiceMetrics{
					{Name: "a", Metrics: map[string]float64{"m": -5}},
				},
			},
			metricName: "m",
			wantValue:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := agg.Aggregate(tt.snapshot, AggregateInput{
				MetricName:   tt.metricName,
				MetricSource: tt.metricSource,
				Threshold:    tt.threshold,
			})
			if tt.wantErr {
				if tt.wantErrMsg != "" {
					assert.EqualError(t, err, tt.wantErrMsg)
				} else {
					assert.Error(t, err)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantValue, got)
		})
	}
}
