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

package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const workspaceCreatedByLabel = "inferenceset.kaito.sh/created-by"

func newScraperFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, kaitov1alpha1.AddToScheme(scheme))
	assert.NoError(t, kaitov1beta1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newInferenceSet(name, namespace string) *kaitov1alpha1.InferenceSet {
	return &kaitov1alpha1.InferenceSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func newWorkspaceForIS(name, namespace, isName string) *kaitov1beta1.Workspace {
	return &kaitov1beta1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{workspaceCreatedByLabel: isName},
		},
	}
}

// newTestServerURLBuilder returns a urlBuilder that rewrites the scrape target
// from the in-cluster FQDN form to the httptest server URL, encoding the
// requested service into the request path so that per-service responses can
// be keyed on it.
func newTestServerURLBuilder(base string) func(protocol, name, namespace, port, path string) string {
	return func(_, name, namespace, _, _ string) string {
		return fmt.Sprintf("%s/services/%s/%s/metrics", base, namespace, name)
	}
}

func TestVLLMScraper_Scrape(t *testing.T) {
	// Test server returns valid prometheus text per workspace name. ws-fail returns 500.
	mux := http.NewServeMux()
	mux.HandleFunc("/services/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/services/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		name := parts[1]
		switch name {
		case "ws-fail":
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case "ws-a":
			// gauge with two label sets (sum = 3 + 4 = 7)
			fmt.Fprint(w, `# HELP vllm:num_requests_waiting number of waiting requests
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model="m1"} 3
vllm:num_requests_waiting{model="m2"} 4
# HELP vllm:running_requests running requests
# TYPE vllm:running_requests counter
vllm:running_requests 12
`)
		case "ws-b":
			fmt.Fprint(w, `# HELP vllm:num_requests_waiting number of waiting requests
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting 5
`)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Verify URL construction is sane too.
	if _, err := url.Parse(srv.URL); err != nil {
		t.Fatalf("bad test server URL: %v", err)
	}

	const (
		isName = "is1"
		ns     = "ns1"
	)

	is := newInferenceSet(isName, ns)
	wsA := newWorkspaceForIS("ws-a", ns, isName)
	wsB := newWorkspaceForIS("ws-b", ns, isName)
	wsFail := newWorkspaceForIS("ws-fail", ns, isName)
	// Unrelated workspace in same namespace to make sure label selector filters it out.
	wsOther := newWorkspaceForIS("ws-other", ns, "some-other-is")

	s := NewVLLMScraper(newScraperFakeClient(t, is, wsA, wsB, wsFail, wsOther))
	s.urlBuilder = newTestServerURLBuilder(srv.URL)

	cfg := ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics", Timeout: 2 * time.Second}
	snap, err := s.Scrape(context.Background(), is, cfg)
	assert.NoError(t, err)
	assert.NotNil(t, snap)
	assert.Equal(t, ns, snap.InferenceSet.Namespace)
	assert.Equal(t, isName, snap.InferenceSet.Name)
	assert.Len(t, snap.Services, 3, "should only include workspaces created by this InferenceSet")

	byName := map[string]ServiceMetrics{}
	for _, sm := range snap.Services {
		byName[sm.Name] = sm
	}

	assert.NoError(t, byName["ws-a"].Err)
	assert.Equal(t, float64(7), byName["ws-a"].Metrics["vllm:num_requests_waiting"])
	assert.Equal(t, float64(12), byName["ws-a"].Metrics["vllm:running_requests"])

	assert.NoError(t, byName["ws-b"].Err)
	assert.Equal(t, float64(5), byName["ws-b"].Metrics["vllm:num_requests_waiting"])

	assert.Error(t, byName["ws-fail"].Err)
	assert.Nil(t, byName["ws-fail"].Metrics)
}

func TestVLLMScraper_Scrape_NoWorkspaces(t *testing.T) {
	is := newInferenceSet("is1", "ns1")
	s := NewVLLMScraper(newScraperFakeClient(t, is))
	s.urlBuilder = newTestServerURLBuilder("http://unused")
	snap, err := s.Scrape(context.Background(), is, ScrapeConfig{Protocol: "http", Port: "80", Path: "/metrics", Timeout: time.Second})
	assert.NoError(t, err)
	assert.NotNil(t, snap)
	assert.Empty(t, snap.Services)
}

func TestDefaultURLBuilder(t *testing.T) {
	got := defaultURLBuilder("http", "svc", "ns", "80", "/metrics")
	assert.Equal(t, "http://svc.ns.svc.cluster.local:80/metrics", got)
}
