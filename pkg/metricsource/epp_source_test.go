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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newEPPFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newEPPPod(name, namespace, eppName, podIP string, phase corev1.PodPhase) *corev1.Pod {
	return newEPPPodWithLabels(name, namespace, map[string]string{eppNameLabel: eppName}, podIP, phase)
}

func newEPPPodWithLabels(name, namespace string, labels map[string]string, podIP string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: podIP},
	}
}

// The derived name is the only handle on the EPP pods, so it has to reproduce
// the production-stack ModelDeployment chart's label value byte for byte.
func TestEPPName(t *testing.T) {
	tests := []struct {
		name             string
		inferenceSetName string
		want             string
	}{
		{
			name:             "name is suffixed",
			inferenceSetName: "phi-4-mini",
			want:             "phi-4-mini-inferencepool-epp",
		},
		{
			name:             "surrounding whitespace is trimmed",
			inferenceSetName: " phi-4-mini ",
			want:             "phi-4-mini-inferencepool-epp",
		},
		{
			name:             "long name is not truncated",
			inferenceSetName: strings.Repeat("b", 60),
			want:             strings.Repeat("b", 60) + "-inferencepool-epp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EPPName(tt.inferenceSetName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEPPSource_Name(t *testing.T) {
	assert.Equal(t, EPPSourceName, NewEPPSource(newEPPFakeClient(t)).Name())
}

func TestEPPSource_Scrape(t *testing.T) {
	// Serves per-pod payloads keyed on the path segment the test URL builder
	// encodes, so a snapshot can be attributed back to the right pod.
	mux := http.NewServeMux()
	mux.HandleFunc("/pods/", func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		switch strings.TrimPrefix(r.URL.Path, "/pods/") {
		case "10.0.0.1":
			fmt.Fprint(w, `# TYPE inference_pool_per_pod_queue_size gauge
inference_pool_per_pod_queue_size{model_server_pod="p1"} 2
inference_pool_per_pod_queue_size{model_server_pod="p2"} 3
`)
		case "10.0.0.2":
			fmt.Fprint(w, `# TYPE inference_pool_per_pod_queue_size gauge
inference_pool_per_pod_queue_size{model_server_pod="p3"} 4
`)
		case "10.0.0.9":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	newSource := func(c client.Client) *EPPSource {
		s := NewEPPSource(c)
		s.urlBuilder = func(_, podIP, _, _ string) string {
			return fmt.Sprintf("%s/pods/%s", server.URL, podIP)
		}
		return s
	}

	is := newInferenceSet("is1", "ns1")
	eppName := EPPName(is.Name)
	cfg := ScrapeConfig{Protocol: "http", Port: "9090", Path: "/metrics", Timeout: 3 * time.Second}

	t.Run("scrapes every running EPP pod", func(t *testing.T) {
		c := newEPPFakeClient(t,
			newEPPPod("epp-1", "ns1", eppName, "10.0.0.1", corev1.PodRunning),
			newEPPPod("epp-2", "ns1", eppName, "10.0.0.2", corev1.PodRunning),
		)
		snap, err := newSource(c).Scrape(context.Background(), is, cfg)
		assert.NoError(t, err)
		assert.Len(t, snap.Services, 2)

		// Each pod contributes its own gauge total; the aggregator sums them.
		byName := map[string]float64{}
		for _, sm := range snap.Services {
			assert.NoError(t, sm.Err)
			byName[sm.Name] = sm.Metrics["inference_pool_per_pod_queue_size"]
		}
		assert.Equal(t, float64(5), byName["epp-1"])
		assert.Equal(t, float64(4), byName["epp-2"])
	})

	t.Run("a failing pod is recorded without failing the scrape", func(t *testing.T) {
		c := newEPPFakeClient(t,
			newEPPPod("epp-1", "ns1", eppName, "10.0.0.1", corev1.PodRunning),
			newEPPPod("epp-bad", "ns1", eppName, "10.0.0.9", corev1.PodRunning),
		)
		snap, err := newSource(c).Scrape(context.Background(), is, cfg)
		assert.NoError(t, err)
		assert.Len(t, snap.Services, 2)

		errs := map[string]bool{}
		for _, sm := range snap.Services {
			errs[sm.Name] = sm.Err != nil
		}
		assert.False(t, errs["epp-1"])
		assert.True(t, errs["epp-bad"])
	})

	t.Run("pods that cannot be scraped yet are excluded", func(t *testing.T) {
		// A pending pod or one without an IP has nothing listening; including it
		// would record a connection error and make a healthy EPP look degraded
		// during a rollout.
		c := newEPPFakeClient(t,
			newEPPPod("epp-1", "ns1", eppName, "10.0.0.1", corev1.PodRunning),
			newEPPPod("epp-pending", "ns1", eppName, "10.0.0.3", corev1.PodPending),
			newEPPPod("epp-noip", "ns1", eppName, "", corev1.PodRunning),
		)
		snap, err := newSource(c).Scrape(context.Background(), is, cfg)
		assert.NoError(t, err)
		assert.Len(t, snap.Services, 1)
		assert.Equal(t, "epp-1", snap.Services[0].Name)
	})

	t.Run("pods of another InferenceSet or namespace are ignored", func(t *testing.T) {
		c := newEPPFakeClient(t,
			newEPPPod("epp-1", "ns1", eppName, "10.0.0.1", corev1.PodRunning),
			newEPPPod("other-epp", "ns1", EPPName("is2"), "10.0.0.2", corev1.PodRunning),
			newEPPPod("far-epp", "ns2", eppName, "10.0.0.2", corev1.PodRunning),
		)
		snap, err := newSource(c).Scrape(context.Background(), is, cfg)
		assert.NoError(t, err)
		assert.Len(t, snap.Services, 1)
		assert.Equal(t, "epp-1", snap.Services[0].Name)
	})

	t.Run("no EPP pod yields an empty snapshot rather than an error", func(t *testing.T) {
		// KAITO only creates an EPP under specific conditions and never before
		// the first Workspace exists, so absence is a state the aggregator has
		// to interpret, not a scrape failure.
		snap, err := newSource(newEPPFakeClient(t)).Scrape(context.Background(), is, cfg)
		assert.NoError(t, err)
		assert.NotNil(t, snap)
		assert.Empty(t, snap.Services)
	})
}

func TestDefaultEPPURLBuilder(t *testing.T) {
	assert.Equal(t, "http://10.0.0.1:9090/metrics", defaultEPPURLBuilder("http", "10.0.0.1", "9090", "/metrics"))
	assert.Equal(t, "https://10.0.0.1:9090/metrics", defaultEPPURLBuilder("https", "10.0.0.1", "9090", "/metrics"))
	// An IPv6 pod IP has to be bracketed or the port would parse as part of it.
	assert.Equal(t, "http://[fd00::1]:9090/metrics", defaultEPPURLBuilder("", "fd00::1", "9090", "/metrics"))
}
