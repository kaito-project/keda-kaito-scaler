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

// Package scraper provides abstractions for scraping inference metrics that
// drive KEDA scaling decisions.
//
// The package follows a per-call snapshot model: each invocation of Scraper.Scrape
// returns a point-in-time MetricSnapshot for all services belonging to a given
// InferenceSet. The snapshot carries every metric parsed from each service, so
// the caller (typically the KEDA external scaler) can pick one or more metric
// names from the same snapshot without issuing additional network requests.
package scraper

import (
	"context"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"k8s.io/apimachinery/pkg/types"
)

// ScrapeConfig controls how a Scraper connects to an individual service.
type ScrapeConfig struct {
	// Protocol is either "http" or "https".
	Protocol string
	// Port is the service port exposing the Prometheus metrics endpoint.
	Port string
	// Path is the HTTP path of the metrics endpoint (e.g. "/metrics").
	Path string
	// Timeout bounds a single per-service scrape.
	Timeout time.Duration
}

// Bucket is a single cumulative histogram bucket: CumulativeCount observations
// were less than or equal to the upper bound Le.
type Bucket struct {
	Le              float64
	CumulativeCount uint64
}

// Histogram captures a Prometheus histogram metric family: its cumulative
// buckets (sorted by Le ascending) plus the overall observation count and sum.
type Histogram struct {
	Buckets []Bucket
	Count   uint64
	Sum     float64
}

// ServiceMetrics captures the metrics scraped from a single service (one
// workspace replica, or a single EPP endpoint) that belongs to an InferenceSet.
type ServiceMetrics struct {
	// Name / Namespace identify the service that was scraped.
	Name      string
	Namespace string
	// Metrics maps a metric family name (e.g. "vllm:num_requests_waiting") to its
	// aggregated scalar value. When a metric family contains multiple label sets
	// (for example one per model), the values are summed. Empty when Err != nil.
	Metrics map[string]float64
	// Series maps a scalar metric family name to the list of its individual
	// per-series values (one entry per label set). Unlike Metrics (which sums
	// label sets), Series preserves each series so aggregators can compute a
	// precise per-series average (e.g. inference_pool_per_pod_queue_size where
	// each series is one backend pod). Populated by scrapers that need per-series
	// fidelity; nil for scrapers that only expose summed scalars.
	Series map[string][]float64
	// Histograms maps a histogram metric family name to its parsed buckets/count/
	// sum, enabling quantile aggregation (e.g. p95 latency). nil when the service
	// exposes no histograms or the scraper does not parse them.
	Histograms map[string]Histogram
	// Err is non-nil when scraping or parsing this specific service failed. Other
	// services in the same snapshot may still have been scraped successfully.
	Err error
}

// MetricSnapshot is a point-in-time collection of per-service metrics for an
// InferenceSet. It is produced by Scraper.Scrape and consumed by an Aggregator.
type MetricSnapshot struct {
	InferenceSet types.NamespacedName
	Services     []ServiceMetrics
	ScrapedAt    time.Time
}

// Scraper collects metrics from every service that backs an InferenceSet and
// returns a MetricSnapshot. Implementations must be safe for concurrent use.
type Scraper interface {
	Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error)
}
