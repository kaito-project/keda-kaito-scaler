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

	"go.uber.org/multierr"
	"k8s.io/klog/v2"

	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
)

// ServiceSumAggregator sums a metric across every service in the snapshot
// without compensating for services that failed to scrape.
//
// It exists alongside SumAggregator, which adds the per-replica threshold once
// per unscrapable service so a partial scrape cannot look like a drop in load.
// That compensation is deliberate for the single-metric AverageValue path, but
// it is wrong for a fleet-wide quantity compared against a fixed threshold in
// composite Value mode (scalingModifiers): there is no meaningful per-replica
// threshold to add. The EPP queue depth is exactly such a quantity -- it is a
// property of the router, not of any replica.
//
// Negative sums are clamped to 0 for the same reason SumAggregator clamps:
// KEDA's external_scaler client silently treats a negative MetricValueFloat as
// 0, which would mask metric source bugs.
type ServiceSumAggregator struct{}

// ServiceSumAggregatorName is the registered name of the ServiceSumAggregator.
const ServiceSumAggregatorName = "service-sum"

// NewServiceSumAggregator returns a ready-to-use ServiceSumAggregator.
func NewServiceSumAggregator() *ServiceSumAggregator {
	return &ServiceSumAggregator{}
}

// Name identifies the ServiceSumAggregator.
func (a *ServiceSumAggregator) Name() string { return ServiceSumAggregatorName }

// Aggregate implements Aggregator. It returns the plain sum of input.MetricName
// across all successfully scraped services, and errors only when no service
// could be scraped at all. The threshold is unused.
//
// Unlike ServiceAverageAggregator, absence of the metric on every scraped
// service is reported as 0 rather than an error. The metrics this aggregator
// serves are labelled vectors that publish no series until the first matching
// event: an idle EPP genuinely exposes no queue-size series, and that idleness
// is the entire state scale-to-zero needs to observe. Erroring there would put
// every parked workload into a permanent TriggerError and freeze it. The cost is
// that a misspelled metric name reads as a steady 0 instead of surfacing an
// error.
func (a *ServiceSumAggregator) Aggregate(snapshot *metricsource.MetricSnapshot, input AggregateInput) (float64, error) {
	metricName := input.MetricName
	if snapshot == nil {
		return 0, fmt.Errorf("metric snapshot is nil")
	}
	if len(snapshot.Services) == 0 {
		if input.MetricSource == metricsource.EPPSourceName {
			return 0, fmt.Errorf(
				"no ready Endpoint Picker pods available for selector %s=%s in namespace %s; "+
					"this is expected temporarily during startup while KAITO creates the first Workspace and Endpoint Picker; "+
					"if it persists, verify the InferenceSet uses a vLLM preset and the Gateway API Inference Extension is enabled",
				metricsource.EPPNameLabel, metricsource.EPPName(snapshot.InferenceSet.Name), snapshot.InferenceSet.Namespace)
		}
		return 0, fmt.Errorf("no services found for inferenceset %s", snapshot.InferenceSet)
	}

	var (
		sum          float64
		successCount int
		errs         []error
	)
	for _, sm := range snapshot.Services {
		if sm.Err != nil {
			errs = append(errs, fmt.Errorf("service %s/%s: %w", sm.Namespace, sm.Name, sm.Err))
			continue
		}
		sum += sm.Metrics[metricName]
		successCount++
	}

	// Nothing was scraped, so the value is unknown rather than 0. Reporting a
	// phantom 0 here would look like an idle fleet and could park a workload
	// that is in fact serving traffic.
	if successCount == 0 {
		if combined := multierr.Combine(errs...); combined != nil {
			return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s: %w", metricName, snapshot.InferenceSet, combined)
		}
		return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s", metricName, snapshot.InferenceSet)
	}

	if sum < 0 {
		klog.Warningf("summed metric %q for inferenceset %s is negative (%f); clamping to 0", metricName, snapshot.InferenceSet, sum)
		sum = 0
	}

	klog.V(4).Infof("summed metric %q for inferenceset %s: sum=%f success=%d total=%d",
		metricName, snapshot.InferenceSet, sum, successCount, len(snapshot.Services))
	return sum, nil
}
