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

	"github.com/kaito-project/keda-kaito-scaler/pkg/scraper"
)

// ServiceAverageAggregator averages a metric across every service (workspace
// replica) that belongs to an InferenceSet, using each service's already-summed
// per-service scalar (scraper.ServiceMetrics.Metrics[metricName]) as the unit of
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
// 0, which would mask scraper bugs.
type ServiceAverageAggregator struct{}

// NewServiceAverageAggregator returns a ready-to-use ServiceAverageAggregator.
func NewServiceAverageAggregator() *ServiceAverageAggregator {
	return &ServiceAverageAggregator{}
}

// Aggregate implements Aggregator. The threshold argument is unused (kept to
// satisfy the Aggregator interface). It returns the arithmetic mean of
// metricName across all successfully scraped services, or an error when no
// service could be scraped successfully.
func (a *ServiceAverageAggregator) Aggregate(snapshot *scraper.MetricSnapshot, metricName string, _ float64) (float64, error) {
	if snapshot == nil {
		return 0, fmt.Errorf("metric snapshot is nil")
	}
	if len(snapshot.Services) == 0 {
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
		// The service was scraped successfully. A missing metric key means this
		// replica currently reports no such activity (e.g. no requests queued),
		// so it contributes 0 and still counts as a real replica instead of being
		// treated as a scrape failure. This keeps cold-start / no-traffic windows
		// from erroring (which would otherwise block composite formulas).
		val, ok := sm.Metrics[metricName]
		if !ok {
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

	avg := sum / float64(successCount)
	if avg < 0 {
		klog.Warningf("averaged metric %q for inferenceset %s is negative (%f); clamping to 0", metricName, snapshot.InferenceSet, avg)
		avg = 0
	}

	klog.V(4).Infof("averaged metric %q for inferenceset %s: avg=%f success=%d total=%d",
		metricName, snapshot.InferenceSet, avg, successCount, len(snapshot.Services))
	return avg, nil
}
