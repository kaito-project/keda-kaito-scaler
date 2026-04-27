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

package metrics

import (
	"fmt"

	"go.uber.org/multierr"
	"k8s.io/klog/v2"
)

// Aggregator reduces the per-service values inside a MetricSnapshot to the
// single metric value that KEDA consumes.
//
// The returned value is meant to be paired with HPA's "AverageValue" target
// type, where the threshold passed by KEDA represents the per-replica desired
// load. HPA then computes desiredReplicas = ceil(value / threshold), so the
// aggregator must return the *total* (sum) load across all services.
type Aggregator interface {
	Aggregate(snapshot *MetricSnapshot, metricName string, threshold float64) (float64, error)
}

// SumAggregator sums a metric across every service that belongs to an
// InferenceSet, compensating for services that could not be scraped in order
// to avoid flapping.
//
// Compensation strategy (mirrors how K8s HPA's ReplicaCalculator handles
// missing pods for Pods/Resource metrics, which it does NOT do for External
// metrics – so we do it ourselves):
//
//   - scale-up direction (avg of successful samples >= threshold): missing
//     services contribute 0 (their absence must not prevent scale-up).
//   - scale-down direction (avg of successful samples <  threshold): missing
//     services contribute the threshold value (their absence must not trigger
//     further scale-down).
//
// If no service could be scraped successfully, the combined per-service errors
// are returned.
//
// Negative aggregated values are clamped to 0; KEDA's external_scaler client
// otherwise silently treats negative MetricValueFloat as 0 by falling back to
// the deprecated int64 MetricValue, which would mask bugs in the scraper.
type SumAggregator struct{}

// NewSumAggregator returns a ready-to-use SumAggregator.
func NewSumAggregator() *SumAggregator {
	return &SumAggregator{}
}

// Aggregate implements Aggregator.
func (a *SumAggregator) Aggregate(snapshot *MetricSnapshot, metricName string, threshold float64) (float64, error) {
	if snapshot == nil {
		return 0, fmt.Errorf("metric snapshot is nil")
	}
	total := len(snapshot.Services)
	if total == 0 {
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
		val, ok := sm.Metrics[metricName]
		if !ok {
			errs = append(errs, fmt.Errorf("service %s/%s: metric %q not found", sm.Namespace, sm.Name, metricName))
			continue
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

	// Compensate for missing services only in the scale-down direction.
	if successCount != total {
		avgSuccess := sum / float64(successCount)
		if avgSuccess < threshold {
			sum += threshold * float64(total-successCount)
		}
	}

	if sum < 0 {
		klog.Warningf("aggregated metric %q for inferenceset %s is negative (%f); clamping to 0", metricName, snapshot.InferenceSet, sum)
		sum = 0
	}

	klog.V(4).Infof("aggregated metric %q for inferenceset %s: sum=%f success=%d total=%d threshold=%f",
		metricName, snapshot.InferenceSet, sum, successCount, total, threshold)
	return sum, nil
}
