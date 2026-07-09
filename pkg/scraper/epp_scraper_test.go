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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newEPPScraperFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	assert.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newEPPService(name, namespace, isName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				eppInferenceSetLabel: isName,
				eppOwnedByLabel:      eppOwnedByValue,
			},
		},
	}
}

const eppMetricsBody = `# HELP inference_pool_per_pod_queue_size per pod queue size
# TYPE inference_pool_per_pod_queue_size gauge
inference_pool_per_pod_queue_size{name="pool",model_server_pod="pod-a"} 2
inference_pool_per_pod_queue_size{name="pool",model_server_pod="pod-b"} 1
# HELP inference_pool_ready_pods ready pods
# TYPE inference_pool_ready_pods gauge
inference_pool_ready_pods 2
# HELP inference_objective_request_duration_seconds request duration
# TYPE inference_objective_request_duration_seconds histogram
inference_objective_request_duration_seconds_bucket{le="1.0"} 90
inference_objective_request_duration_seconds_bucket{le="1.25"} 99
inference_objective_request_duration_seconds_bucket{le="1.5"} 105
inference_objective_request_duration_seconds_bucket{le="+Inf"} 105
inference_objective_request_duration_seconds_sum 62.45
inference_objective_request_duration_seconds_count 105
`

func TestEPPScraper_Scrape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, eppMetricsBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const (
		isName = "is1"
		ns     = "ns1"
	)
	is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns}}
	svc := newEPPService("is1-inferencepool-epp", ns, isName)
	// Unrelated service to ensure label selectivity.
	other := newEPPService("other-epp", ns, "other-is")

	s := NewEPPScraper(newEPPScraperFakeClient(t, is, svc, other))
	s.urlBuilder = func(_, _, _, _, _ string) string { return srv.URL + "/metrics" }

	snap, err := s.Scrape(context.Background(), is, ScrapeConfig{Timeout: 2 * time.Second})
	assert.NoError(t, err)
	assert.Len(t, snap.Services, 1)

	sm := snap.Services[0]
	assert.NoError(t, sm.Err)
	assert.Equal(t, "is1-inferencepool-epp", sm.Name)

	// Per-pod gauge exposed both as summed scalar and as a per-series list.
	assert.ElementsMatch(t, []float64{2, 1}, sm.Series["inference_pool_per_pod_queue_size"])
	assert.Equal(t, 3.0, sm.Metrics["inference_pool_per_pod_queue_size"])
	assert.Equal(t, 2.0, sm.Metrics["inference_pool_ready_pods"])

	// Histogram parsed with buckets/count/sum.
	h, ok := sm.Histograms["inference_objective_request_duration_seconds"]
	assert.True(t, ok)
	assert.Equal(t, uint64(105), h.Count)
	assert.InDelta(t, 62.45, h.Sum, 1e-9)
	assert.Len(t, h.Buckets, 4)
	assert.Equal(t, 1.0, h.Buckets[0].Le)
	assert.True(t, math.IsInf(h.Buckets[3].Le, 1))
	assert.Equal(t, uint64(90), h.Buckets[0].CumulativeCount)
}

func TestEPPScraper_Scrape_NoService(t *testing.T) {
	is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Name: "is1", Namespace: "ns1"}}
	s := NewEPPScraper(newEPPScraperFakeClient(t, is))
	_, err := s.Scrape(context.Background(), is, ScrapeConfig{Timeout: time.Second})
	assert.Error(t, err)
}

func TestEPPScraper_Scrape_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Name: "is1", Namespace: "ns1"}}
	svc := newEPPService("is1-inferencepool-epp", "ns1", "is1")
	s := NewEPPScraper(newEPPScraperFakeClient(t, is, svc))
	s.urlBuilder = func(_, _, _, _, _ string) string { return srv.URL + "/metrics" }

	// Discovery succeeds, so Scrape returns no error but records a per-service err.
	snap, err := s.Scrape(context.Background(), is, ScrapeConfig{Timeout: time.Second})
	assert.NoError(t, err)
	assert.Len(t, snap.Services, 1)
	assert.Error(t, snap.Services[0].Err)
}
