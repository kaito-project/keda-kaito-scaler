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

package scraper

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/util/promscrape"
)

// +kubebuilder:rbac:groups="",resources=services,verbs=list;watch

const (
	// eppInferenceSetLabel and eppOwnedByLabel identify the EndpointPicker (EPP)
	// Service that llm-d/GAIE runs for an InferenceSet's InferencePool. The EPP
	// exposes the queue-length and request-latency metrics we consume.
	eppInferenceSetLabel = "kaito.sh/inferenceset"
	eppOwnedByLabel      = "kaito.sh/owned-by"
	eppOwnedByValue      = "modeldeployment"

	// Default EPP metrics endpoint. The EPP serves plain-text Prometheus metrics
	// on the http-metrics port (9090) at /metrics with no authentication. These
	// are used when the ScrapeConfig does not override Port/Path.
	defaultEPPMetricsPort = "9090"
	defaultEPPMetricsPath = "/metrics"
)

// EPPScraper scrapes the Prometheus /metrics endpoint exposed by the
// EndpointPicker (EPP) Service associated with an InferenceSet. It implements
// the Scraper interface and returns a single-service MetricSnapshot describing
// that EPP endpoint, populating per-series gauges (via Series) and histograms
// (via Histograms) in addition to summed scalars (via Metrics).
type EPPScraper struct {
	kubeClient client.Client
	transports *promscrape.Transports
	// urlBuilder produces the scrape URL for a given EPP service. Overridable in
	// tests; defaults to the in-cluster FQDN.
	urlBuilder func(protocol, name, namespace, port, path string) string
}

// NewEPPScraper constructs an EPPScraper that reuses a small HTTP connection
// pool for plain-text scraping and a separate one for https.
func NewEPPScraper(kubeClient client.Client) *EPPScraper {
	return &EPPScraper{
		kubeClient: kubeClient,
		transports: promscrape.NewTransports(),
		urlBuilder: promscrape.DefaultURLBuilder,
	}
}

// Scrape discovers the EPP Service for the InferenceSet by label, scrapes its
// /metrics endpoint, and returns a MetricSnapshot with a single ServiceMetrics
// entry for that endpoint. Scrape returns an error only when EPP discovery
// fails; a per-endpoint scrape/parse error is recorded on the ServiceMetrics.
func (s *EPPScraper) Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error) {
	svc, err := s.discoverEPPService(ctx, is)
	if err != nil {
		return nil, fmt.Errorf("failed to discover EPP service for InferenceSet %s/%s: %w", is.Namespace, is.Name, err)
	}

	// EPP defaults differ from the workspace Service defaults, so fill them in
	// when the caller left them at the generic defaults / empty.
	eppCfg := cfg
	if eppCfg.Port == "" || eppCfg.Port == "80" {
		eppCfg.Port = defaultEPPMetricsPort
	}
	if eppCfg.Path == "" {
		eppCfg.Path = defaultEPPMetricsPath
	}
	if eppCfg.Protocol == "" {
		eppCfg.Protocol = "http"
	}

	snap := &MetricSnapshot{
		InferenceSet: types.NamespacedName{Namespace: is.Namespace, Name: is.Name},
		ScrapedAt:    time.Now(),
		Services:     make([]ServiceMetrics, 0, 1),
	}

	sm := ServiceMetrics{Name: svc.Name, Namespace: svc.Namespace}
	metricsMap, series, histograms, scrapeErr := s.scrapeEPP(ctx, svc.Name, svc.Namespace, eppCfg)
	if scrapeErr != nil {
		klog.Errorf("failed to scrape EPP metrics from service %s/%s: %v", svc.Namespace, svc.Name, scrapeErr)
		sm.Err = scrapeErr
	} else {
		sm.Metrics = metricsMap
		sm.Series = series
		sm.Histograms = histograms
	}
	snap.Services = append(snap.Services, sm)
	return snap, nil
}

// discoverEPPService finds the EPP Service for the InferenceSet by label. The
// EPP is deployed one-per-InferenceSet and labeled with the InferenceSet name.
func (s *EPPScraper) discoverEPPService(ctx context.Context, is *kaitov1beta1.InferenceSet) (*corev1.Service, error) {
	var svcList corev1.ServiceList
	selector := labels.SelectorFromSet(labels.Set{
		eppInferenceSetLabel: is.Name,
		eppOwnedByLabel:      eppOwnedByValue,
	})
	if err := s.kubeClient.List(ctx, &svcList,
		client.InNamespace(is.Namespace),
		&client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return nil, err
	}

	switch len(svcList.Items) {
	case 0:
		return nil, fmt.Errorf("no EPP service found with labels %s=%s,%s=%s in namespace %s",
			eppInferenceSetLabel, is.Name, eppOwnedByLabel, eppOwnedByValue, is.Namespace)
	case 1:
		return &svcList.Items[0], nil
	default:
		// Deterministically pick the first by name so repeated scrapes are stable.
		sort.Slice(svcList.Items, func(i, j int) bool {
			return svcList.Items[i].Name < svcList.Items[j].Name
		})
		klog.Warningf("found %d EPP services for InferenceSet %s/%s; using %s",
			len(svcList.Items), is.Namespace, is.Name, svcList.Items[0].Name)
		return &svcList.Items[0], nil
	}
}

func (s *EPPScraper) scrapeEPP(ctx context.Context, name, namespace string, cfg ScrapeConfig) (map[string]float64, map[string][]float64, map[string]Histogram, error) {
	httpClient := s.transports.ClientFor(cfg.Protocol, cfg.Timeout)

	url := s.urlBuilder(cfg.Protocol, name, namespace, cfg.Port, cfg.Path)
	klog.V(6).Infof("scraping EPP metrics from service %s/%s: %s", namespace, name, url)

	families, err := promscrape.FetchMetricFamilies(ctx, httpClient, url)
	if err != nil {
		return nil, nil, nil, err
	}

	metricsMap := make(map[string]float64)
	series := make(map[string][]float64)
	histograms := make(map[string]Histogram)

	for familyName, mf := range families {
		if mf.GetType() == dto.MetricType_HISTOGRAM {
			if h, ok := mergeHistogram(mf); ok {
				histograms[familyName] = h
			}
			continue
		}

		var (
			sum   float64
			found bool
			vals  []float64
		)
		for _, m := range mf.GetMetric() {
			v, ok := promscrape.ExtractScalarValue(m)
			if !ok {
				continue
			}
			sum += v
			vals = append(vals, v)
			found = true
		}
		if found {
			metricsMap[familyName] = sum
			series[familyName] = vals
		}
	}

	return metricsMap, series, histograms, nil
}

// mergeHistogram merges all label-set series of a histogram family into a
// single cumulative histogram (bucket counts summed per upper bound, plus total
// count and sum). This lets a quantile aggregator operate on one EPP endpoint
// that exposes a histogram split across multiple label sets.
func mergeHistogram(mf *dto.MetricFamily) (Histogram, bool) {
	bucketCounts := make(map[float64]uint64)
	var totalCount uint64
	var totalSum float64
	found := false

	for _, m := range mf.GetMetric() {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		found = true
		totalCount += h.GetSampleCount()
		totalSum += h.GetSampleSum()
		for _, b := range h.GetBucket() {
			bucketCounts[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}
	if !found {
		return Histogram{}, false
	}

	buckets := make([]Bucket, 0, len(bucketCounts))
	for le, c := range bucketCounts {
		buckets = append(buckets, Bucket{Le: le, CumulativeCount: c})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Le < buckets[j].Le })

	// If multiple series were summed, the +Inf bucket may not equal totalCount
	// due to per-series merging; prefer the +Inf bucket count when present so
	// quantile interpolation is consistent with the bucket ladder.
	if len(buckets) > 0 {
		last := buckets[len(buckets)-1]
		if math.IsInf(last.Le, 1) {
			totalCount = last.CumulativeCount
		}
	}

	return Histogram{Buckets: buckets, Count: totalCount, Sum: totalSum}, true
}
