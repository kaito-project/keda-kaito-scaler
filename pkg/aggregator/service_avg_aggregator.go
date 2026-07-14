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

// ServiceAverageAggregator averages a metric across every service (workspace
// replica) that belongs to an InferenceSet, using each service's already-summed
// per-service scalar (metricsource.ServiceMetrics.Metrics[metricName]) as the unit of
// averaging.
//
// Unlike PerPodAverageAggregator, which divides by the number of per-label-set
// series, this aggregator divides by the number of successfully scraped
// services, so the result is a true per-replica average that is independent of
// how many label sets (e.g. multiple models) a single service exposes.
//
// It is meant to be paired with a fixed-threshold "Value" comparison (composite
// scalingModifiers), where every metric variable must be replica-count
// independent so a user-set threshold keeps the same per-replica meaning as the
// replica count changes.
//
// Negative averages are clamped to 0 for the same reason SumAggregator clamps:
// KEDA's external_scaler client silently treats a negative MetricValueFloat as
// 0, which would mask metric source bugs.
type ServiceAverageAggregator struct{}

// ServiceAverageAggregatorName is the registered name of the ServiceAverageAggregator.
const ServiceAverageAggregatorName = "service-avg"

// NewServiceAverageAggregator returns a ready-to-use ServiceAverageAggregator.
func NewServiceAverageAggregator() *ServiceAverageAggregator {
	return &ServiceAverageAggregator{}
}

// Name identifies the ServiceAverageAggregator.
func (a *ServiceAverageAggregator) Name() string { return ServiceAverageAggregatorName }

// Aggregate implements Aggregator. It returns the arithmetic mean of
// input.MetricName across all successfully scraped services. A service scraped
// without the metric contributes 0 as long as at least one service exposes it.
// It returns an error when no service could be scraped successfully, or when the
// metric is exposed by no scraped service (treated as unknown rather than a
// phantom 0). The threshold is unused.
func (a *ServiceAverageAggregator) Aggregate(snapshot *metricsource.MetricSnapshot, input AggregateInput) (float64, error) {
	metricName := input.MetricName
	if snapshot == nil {
		return 0, fmt.Errorf("metric snapshot is nil")
	}
	if len(snapshot.Services) == 0 {
		return 0, fmt.Errorf("no services found for inferenceset %s", snapshot.InferenceSet)
	}

	var (
		sum          float64
		successCount int
		found        bool
		errs         []error
	)
	for _, sm := range snapshot.Services {
		if sm.Err != nil {
			errs = append(errs, fmt.Errorf("service %s/%s: %w", sm.Namespace, sm.Name, sm.Err))
			continue
		}
		// The service was scraped successfully. A metric key missing on *some*
		// (but not all) services means that replica currently reports no such
		// activity (e.g. no requests queued), so it contributes 0 and still counts
		// as a real replica. Absence on *every* scraped service is handled as
		// unknown below (see the !found check) rather than a genuine 0.
		val, ok := sm.Metrics[metricName]
		if ok {
			found = true
		} else {
			klog.V(4).Infof("metric %q absent on scraped service %s/%s; treating as 0", metricName, sm.Namespace, sm.Name)
		}
		sum += val
		successCount++
	}

	if successCount == 0 {
		if combined := multierr.Combine(errs...); combined != nil {
			return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s: %w", metricName, snapshot.InferenceSet, combined)
		}
		return 0, fmt.Errorf("failed to resolve metric %q for inferenceset %s", metricName, snapshot.InferenceSet)
	}

	// The metric was absent on every service that scraped successfully: this is a
	// misconfiguration (wrong metric name, or a metric the pods do not expose)
	// rather than a genuine 0. Report it as unknown so KEDA holds the current
	// replica count instead of scaling down on a phantom 0.
	if !found {
		return 0, fmt.Errorf("metric %q not exposed by any scraped service for inferenceset %s", metricName, snapshot.InferenceSet)
	}

	avg := sum / float64(successCount)
	if avg < 0 {
		klog.Warningf("averaged metric %q for inferenceset %s is negative (%f); clamping to 0", metricName, snapshot.InferenceSet, avg)
		avg = 0
	}

	klog.V(4).Infof("averaged metric %q for inferenceset %s: avg=%f success=%d total=%d",
		metricName, snapshot.InferenceSet, avg, successCount, len(snapshot.Services))
	return avg, nil
}
