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

package promscrape

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

func TestDefaultURLBuilder(t *testing.T) {
	got := DefaultURLBuilder("http", "svc", "ns", "80", "/metrics")
	assert.Equal(t, "http://svc.ns.svc.cluster.local:80/metrics", got)
}

func TestExtractScalarValue(t *testing.T) {
	gauge := &dto.Metric{Gauge: &dto.Gauge{Value: ptr.To(1.5)}}
	v, ok := ExtractScalarValue(gauge)
	assert.True(t, ok)
	assert.Equal(t, 1.5, v)

	counter := &dto.Metric{Counter: &dto.Counter{Value: ptr.To(2.0)}}
	v, ok = ExtractScalarValue(counter)
	assert.True(t, ok)
	assert.Equal(t, 2.0, v)

	untyped := &dto.Metric{Untyped: &dto.Untyped{Value: ptr.To(3.0)}}
	v, ok = ExtractScalarValue(untyped)
	assert.True(t, ok)
	assert.Equal(t, 3.0, v)

	// Histograms and summaries are skipped.
	_, ok = ExtractScalarValue(&dto.Metric{Histogram: &dto.Histogram{}})
	assert.False(t, ok)
}

func TestFetchMetricFamilies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `# HELP vllm:num_requests_waiting number of waiting requests
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting 5
`)
	}))
	defer srv.Close()

	transports := NewTransports()
	client := transports.ClientFor("http", 2*time.Second)

	families, err := FetchMetricFamilies(context.Background(), client, srv.URL)
	assert.NoError(t, err)

	mf, ok := families["vllm:num_requests_waiting"]
	assert.True(t, ok)
	v, ok := ExtractScalarValue(mf.GetMetric()[0])
	assert.True(t, ok)
	assert.Equal(t, 5.0, v)
}

func TestFetchMetricFamilies_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewTransports().ClientFor("http", time.Second)
	_, err := FetchMetricFamilies(context.Background(), client, srv.URL)
	assert.Error(t, err)
}
