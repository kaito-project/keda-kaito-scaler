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

// ServiceMetricsScraper scrapes the Prometheus /metrics endpoint exposed by
// each workspace Service (e.g. the vLLM inference server). Services are
// discovered via the Workspace objects owned by the InferenceSet.
type ServiceMetricsScraper struct {
	kubeClient client.Client
	transports *promscrape.Transports
	// urlBuilder produces the scrape URL for a given service. Overridable in
	// tests; defaults to the in-cluster FQDN.
	urlBuilder func(protocol, name, namespace, port, path string) string
}

// NewServiceMetricsScraper constructs a ServiceMetricsScraper that reuses a
// small HTTP connection pool for plain-text scraping and a separate one for
// https with InsecureSkipVerify (matching the previous scaler behaviour).
func NewServiceMetricsScraper(kubeClient client.Client) *ServiceMetricsScraper {
	return &ServiceMetricsScraper{
		kubeClient: kubeClient,
		transports: promscrape.NewTransports(),
		urlBuilder: promscrape.DefaultURLBuilder,
	}
}

// Scrape iterates over every Workspace owned by the InferenceSet and scrapes
// its /metrics endpoint. A per-service error is recorded on the corresponding
// ServiceMetrics entry; Scrape itself only returns an error when workspace
// discovery fails.
func (s *ServiceMetricsScraper) Scrape(ctx context.Context, is *kaitov1beta1.InferenceSet, cfg ScrapeConfig) (*MetricSnapshot, error) {
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
		m, scrapeErr := s.scrapeService(ctx, ws.Name, ws.Namespace, cfg)
		if scrapeErr != nil {
			klog.Errorf("failed to scrape metrics from service %s/%s: %v", ws.Namespace, ws.Name, scrapeErr)
		}
		snap.Services = append(snap.Services, ServiceMetrics{
			Name:      ws.Name,
			Namespace: ws.Namespace,
			Metrics:   m,
			Err:       scrapeErr,
		})
	}

	return snap, nil
}

func (s *ServiceMetricsScraper) scrapeService(ctx context.Context, name, namespace string, cfg ScrapeConfig) (map[string]float64, error) {
	httpClient := s.transports.ClientFor(cfg.Protocol, cfg.Timeout)

	url := s.urlBuilder(cfg.Protocol, name, namespace, cfg.Port, cfg.Path)
	klog.V(6).Infof("scraping metrics from service %s/%s: %s", namespace, name, url)

	families, err := promscrape.FetchMetricFamilies(ctx, httpClient, url)
	if err != nil {
		return nil, err
	}

	result := make(map[string]float64, len(families))
	for familyName, mf := range families {
		var sum float64
		var found bool
		for _, m := range mf.GetMetric() {
			v, ok := promscrape.ExtractScalarValue(m)
			if !ok {
				continue
			}
			sum += v
			found = true
		}
		if found {
			result[familyName] = sum
		}
	}
	return result, nil
}
