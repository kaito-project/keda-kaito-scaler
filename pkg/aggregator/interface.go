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

// Aggregator reduces the per-service values inside a scraper.MetricSnapshot to the
// single metric value that KEDA consumes.
//
// The returned value is meant to be paired with HPA's "AverageValue" target
// type, where the threshold passed by KEDA represents the per-replica desired
// load. HPA then computes desiredReplicas = ceil(value / threshold), so the
// aggregator must return the *total* (sum) load across all services.
type Aggregator interface {
	Aggregate(snapshot *scraper.MetricSnapshot, metricName string, threshold float64) (float64, error)
}
