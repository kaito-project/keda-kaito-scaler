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

// Package promscrape holds the HTTP scraping and Prometheus text-format parsing
// helpers shared by the concrete scrapers. Keeping the transport pooling,
// request/parse boilerplate, URL construction and scalar extraction here avoids
// duplicating them across scraper implementations.
package promscrape

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Transports bundles the reusable HTTP transports used for scraping: a
// plain-text transport and a separate TLS transport with InsecureSkipVerify
// (matching the scaler's behaviour against in-cluster inference endpoints). Both
// share a small idle-connection pool.
type Transports struct {
	plain *http.Transport
	tls   *http.Transport
}

// NewTransports constructs the plain-text and TLS transports with a small shared
// idle-connection pool.
func NewTransports() *Transports {
	return &Transports{
		plain: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
		tls: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// ClientFor returns an *http.Client using the transport that matches protocol:
// the TLS transport for "https", the plain transport otherwise.
func (t *Transports) ClientFor(protocol string, timeout time.Duration) *http.Client {
	transport := t.plain
	if protocol == "https" {
		transport = t.tls
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// DefaultURLBuilder builds the in-cluster FQDN scrape URL for a Service.
//
// NOTE: Kaito currently exposes the vLLM inference server (and its Prometheus
// /metrics endpoint) as plain HTTP on the workspace Service. The protocol is
// kept configurable so that https can be used if Kaito enables TLS on the
// inference server in the future.
func DefaultURLBuilder(protocol, name, namespace, port, path string) string {
	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%s%s", protocol, name, namespace, port, path)
}

// FetchMetricFamilies performs a GET against url using httpClient and parses the
// Prometheus text exposition format into metric families keyed by family name.
// A non-2xx response is turned into an error after draining a bounded amount of
// the body so the keep-alive connection can be reused by the transport pool.
func FetchMetricFamilies(ctx context.Context, httpClient *http.Client, url string) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build scrape request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded amount of the response body so that the underlying
		// TCP/TLS connection can be reused by the transport keep-alive pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, url)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Prometheus text metrics: %w", err)
	}
	return families, nil
}

// ExtractScalarValue extracts a single scalar value from a *dto.Metric for the
// metric types inference servers commonly expose (gauges, counters, untyped).
// Complex types such as histograms and summaries are skipped.
func ExtractScalarValue(m *dto.Metric) (float64, bool) {
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue(), true
	case m.Counter != nil:
		return m.GetCounter().GetValue(), true
	case m.Untyped != nil:
		return m.GetUntyped().GetValue(), true
	default:
		return 0, false
	}
}
