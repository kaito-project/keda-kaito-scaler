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
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kaito-project/keda-kaito-scaler/pkg/util/inferenceset"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/promscrape"
)

// +kubebuilder:rbac:groups="kaito.sh",resources=inferencesets,verbs=get;list;watch
// +kubebuilder:rbac:groups="kaito.sh",resources=workspaces,verbs=list;watch

// ModelPodSource scrapes the Prometheus /metrics endpoint exposed by
// each workspace Service (e.g. the vLLM inference server). Services are
// discovered via the Workspace objects owned by the InferenceSet.
type ModelPodSource struct {
	kubeClient client.Client
	transports *promscrape.Transports
	// urlBuilder produces the scrape URL for a given service. Overridable in
	// tests; defaults to the in-cluster FQDN.
	urlBuilder func(protocol, name, namespace, port, path string) string
}

// NewModelPodSource constructs a ModelPodSource that reuses a
// small HTTP connection pool for plain-text scraping and a separate one for
// https with InsecureSkipVerify (matching the previous scaler behaviour).
func NewModelPodSource(kubeClient client.Client) *ModelPodSource {
	return &ModelPodSource{
		kubeClient: kubeClient,
		transports: promscrape.NewTransports(),
		urlBuilder: promscrape.DefaultURLBuilder,
	}
}

// Name identifies the source the metrics are scraped from.
func (s *ModelPodSource) Name() string {
	return ModelPodSourceName
}

// Scrape iterates over every Workspace owned by the InferenceSet and scrapes
// its /metrics endpoint. A per-service error is recorded on the corresponding
// ServiceMetrics entry; Scrape itself only returns an error when workspace
// discovery fails.
func (s *ModelPodSource) Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error) {
	workspaceList, err := inferenceset.ListWorkspaces(ctx, is, s.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to list workspaces for InferenceSet %s/%s: %w", is.Namespace, is.Name, err)
	}

	snap := &MetricSnapshot{
		InferenceSet: types.NamespacedName{Namespace: is.Namespace, Name: is.Name},
		ScrapedAt:    time.Now(),
		Services:     make([]ServiceMetrics, 0, len(workspaceList.Items)),
	}

	klog.V(6).Infof("scraping %d workspace service(s) for InferenceSet %s/%s", len(workspaceList.Items), is.Namespace, is.Name)
	for i := range workspaceList.Items {
		ws := &workspaceList.Items[i]
		sm := ServiceMetrics{Name: ws.Name, Namespace: ws.Namespace}
		metricsMap, histograms, scrapeErr := s.scrapeService(ctx, ws.Name, ws.Namespace, cfg)
		if scrapeErr != nil {
			klog.Errorf("failed to scrape metrics from service %s/%s: %v", ws.Namespace, ws.Name, scrapeErr)
			sm.Err = scrapeErr
		} else {
			sm.Metrics = metricsMap
			sm.Histograms = histograms
		}
		snap.Services = append(snap.Services, sm)
	}

	return snap, nil
}

// scrapeService fetches and parses the /metrics endpoint of a single workspace
// Service, returning summed scalars and merged histograms.
func (s *ModelPodSource) scrapeService(ctx context.Context, name, namespace string, cfg ScrapeConfig) (map[string]float64, map[string]Histogram, error) {
	httpClient := s.transports.ClientFor(cfg.Protocol, cfg.Timeout)

	url := s.urlBuilder(cfg.Protocol, name, namespace, cfg.Port, cfg.Path)
	klog.V(6).Infof("scraping metrics from service %s/%s: %s", namespace, name, url)

	families, err := promscrape.FetchMetricFamilies(ctx, httpClient, url)
	if err != nil {
		return nil, nil, err
	}

	metricsMap, histograms := parseFamilies(families)
	return metricsMap, histograms, nil
}
