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

package metricsource

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/util/promscrape"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

const (
	// eppNameSuffix and inferencePoolSuffix mirror the EPP resource naming used
	// by both KAITO's llm-d-router-gateway chart and the ModelDeployment chart.
	inferencePoolSuffix = "-inferencepool"
	eppNameSuffix       = "-epp"

	// eppNameTruncateLength mirrors the `trunc 40` in the llm-d-router-gateway
	// chart (v0.9.0) `_helpers.tpl`. It is reproduced rather than avoided:
	// the EPP Pods carry no untruncated label naming their InferenceSet, so
	// there is nothing else to select on.
	eppNameTruncateLength = 40

	// KAITO's llm-d-router-gateway chart and the ModelDeployment chart use
	// different selector-label conventions for their EPP Pods.
	eppNameLabel                = "llm-d-router-gateway"
	modelDeploymentEPPNameLabel = "inferencepool"

	// defaultEPPMetricsPort and defaultEPPMetricsPath are the EPP's Prometheus
	// endpoint as exposed by the chart.
	defaultEPPMetricsPort = "9090"
	defaultEPPMetricsPath = "/metrics"
)

// EPPNameLabel is the label the chart stamps on the EPP Pods with the derived
// name as its value. Exported so downstream diagnostics can name the selector
// used during discovery.
const EPPNameLabel = eppNameLabel

// EPPSource scrapes the Prometheus /metrics endpoint of every Endpoint Picker
// (EPP) pod KAITO deploys for an InferenceSet.
//
// It scrapes pod IPs directly rather than the EPP Service. The Service is a
// plain ClusterIP, so a Service-FQDN scrape would land on one arbitrary replica,
// whereas the queue depth this source exists to report is only meaningful as a
// sum across every replica.
//
// Unlike ModelPodSource it keeps reporting while the InferenceSet is parked at
// zero replicas, which is what makes an activation threshold observable at all.
type EPPSource struct {
	kubeClient client.Client
	transports *promscrape.Transports
	// urlBuilder produces the scrape URL for a given pod IP. Overridable in
	// tests; defaults to a plain host:port URL.
	urlBuilder func(protocol, podIP, port, path string) string
}

// NewEPPSource constructs an EPPSource sharing the same connection-pool
// behaviour as the other sources.
func NewEPPSource(kubeClient client.Client) *EPPSource {
	return &EPPSource{
		kubeClient: kubeClient,
		transports: promscrape.NewTransports(),
		urlBuilder: defaultEPPURLBuilder,
	}
}

// Name identifies the source the metrics are scraped from.
func (s *EPPSource) Name() string {
	return EPPSourceName
}

// EPPName derives the EPP resource name for an InferenceSet, reproducing the
// llm-d-router-gateway chart's naming exactly.
//
// The chart derives its fullname from the InferencePool name, which KAITO builds
// as `<inferenceset-name>-inferencepool`, then lowercases, trims, truncates to
// 40 characters and appends "-epp". The truncation is applied before the suffix,
// matching the chart, so a long InferenceSet name yields the same result here as
// in the cluster.
//
// Mirrors: llm-d-router-gateway v0.9.0 `_helpers.tpl` and KAITO's
// `pkg/utils/common.go` InferencePoolName.
func EPPName(inferenceSetName string) string {
	base := strings.ToLower(strings.TrimSpace(inferenceSetName + inferencePoolSuffix))
	if len(base) > eppNameTruncateLength {
		base = base[:eppNameTruncateLength]
	}
	return base + eppNameSuffix
}

func modelDeploymentEPPName(inferenceSetName string) string {
	return strings.TrimSpace(inferenceSetName) + inferencePoolSuffix + eppNameSuffix
}

// EPPSelectorDescription lists every supported EPP pod selector for an
// InferenceSet. It is used in diagnostics when neither chart convention finds
// a ready pod.
func EPPSelectorDescription(inferenceSetName string) string {
	return fmt.Sprintf("%s=%s or %s=%s",
		eppNameLabel, EPPName(inferenceSetName),
		modelDeploymentEPPNameLabel, modelDeploymentEPPName(inferenceSetName))
}

// Scrape lists the EPP pods for the InferenceSet and scrapes each one. A
// per-pod error is recorded on the corresponding ServiceMetrics entry; Scrape
// itself only returns an error when pod discovery fails.
//
// Finding no pods is not an error here: KAITO creates the EPP only for a vLLM,
// preset-based InferenceSet with the Gateway API Inference Extension feature
// gate on, and even then not until the first Workspace exists. The empty
// snapshot is passed through so GetMetrics can report the expected selectors.
func (s *EPPSource) Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error) {
	selectors := []map[string]string{
		{eppNameLabel: EPPName(is.Name)},
		{modelDeploymentEPPNameLabel: modelDeploymentEPPName(is.Name)},
	}
	pods := make([]corev1.Pod, 0)
	seen := make(map[types.NamespacedName]struct{})
	for _, selector := range selectors {
		podList := &corev1.PodList{}
		if err := s.kubeClient.List(ctx, podList,
			client.InNamespace(is.Namespace),
			client.MatchingLabels(selector),
		); err != nil {
			return nil, fmt.Errorf("failed to list EPP pods for InferenceSet %s/%s using selector %v: %w", is.Namespace, is.Name, selector, err)
		}
		for i := range podList.Items {
			key := types.NamespacedName{Namespace: podList.Items[i].Namespace, Name: podList.Items[i].Name}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			pods = append(pods, podList.Items[i])
		}
	}

	snap := &MetricSnapshot{
		InferenceSet: types.NamespacedName{Namespace: is.Namespace, Name: is.Name},
		ScrapedAt:    time.Now(),
		Services:     make([]ServiceMetrics, 0, len(pods)),
	}

	for i := range pods {
		pod := &pods[i]
		// A pod that is not Running, or has no IP yet, has nothing to scrape.
		// Including it would record a connection error and make a healthy fleet
		// look partially broken during a rollout.
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		sm := ServiceMetrics{Name: pod.Name, Namespace: pod.Namespace}
		metricsMap, histograms, scrapeErr := s.scrapePod(ctx, pod.Status.PodIP, cfg)
		if scrapeErr != nil {
			klog.Errorf("failed to scrape metrics from EPP pod %s/%s: %v", pod.Namespace, pod.Name, scrapeErr)
			sm.Err = scrapeErr
		} else {
			sm.Metrics = metricsMap
			sm.Histograms = histograms
		}
		snap.Services = append(snap.Services, sm)
	}

	klog.V(6).Infof("scraped %d EPP pod(s) for InferenceSet %s/%s", len(snap.Services), is.Namespace, is.Name)
	return snap, nil
}

// scrapePod fetches and parses the /metrics endpoint of a single EPP pod.
func (s *EPPSource) scrapePod(ctx context.Context, podIP string, cfg ScrapeConfig) (map[string]float64, map[string]Histogram, error) {
	httpClient := s.transports.ClientFor(cfg.Protocol, cfg.Timeout)

	port, path := cfg.Port, cfg.Path
	if port == "" {
		port = defaultEPPMetricsPort
	}
	if path == "" {
		path = defaultEPPMetricsPath
	}

	url := s.urlBuilder(cfg.Protocol, podIP, port, path)
	klog.V(6).Infof("scraping metrics from EPP pod %s: %s", podIP, url)

	families, err := promscrape.FetchMetricFamilies(ctx, httpClient, url)
	if err != nil {
		return nil, nil, err
	}

	metricsMap, histograms := parseFamilies(families)
	return metricsMap, histograms, nil
}

// defaultEPPURLBuilder builds the scrape URL for a pod IP. net.JoinHostPort
// keeps IPv6 addresses bracketed correctly.
func defaultEPPURLBuilder(protocol, podIP, port, path string) string {
	if protocol == "" {
		protocol = "http"
	}
	return fmt.Sprintf("%s://%s%s", protocol, net.JoinHostPort(podIP, port), path)
}
