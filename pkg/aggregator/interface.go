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

import "github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"

// AggregateInput carries the per-request metric metadata an Aggregator needs to
// reduce a snapshot to a single value.
type AggregateInput struct {
	// MetricName is the metric family to aggregate.
	MetricName string
	// Threshold is the per-replica scale threshold; used by sum-style aggregators
	// to compensate for services that could not be scraped. Ignored by others.
	Threshold float64
	// Quantile is the target quantile in (0, 1] for the quantile aggregation.
	// Ignored by non-quantile aggregators.
	Quantile float64
}

// Aggregator reduces the per-service values inside a metricsource.MetricSnapshot
// to the single metric value that KEDA consumes.
//
// The returned value is meant to be paired with HPA's "AverageValue" target
// type, where the threshold passed by KEDA represents the per-replica desired
// load. HPA then computes desiredReplicas = ceil(value / threshold), so the
// aggregator must return the *total* (sum) load across all services.
type Aggregator interface {
	Name() string
	Aggregate(snapshot *metricsource.MetricSnapshot, input AggregateInput) (float64, error)
}
