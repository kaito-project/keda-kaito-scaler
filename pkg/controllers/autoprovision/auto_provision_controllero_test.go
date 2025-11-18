// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package autoprovision

import (
	"fmt"
	"testing"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

func TestGetDefaultKedaKaitoScalerTriggers(t *testing.T) {
	tests := []struct {
		name                  string
		inferenceSetName      string
		inferenceSetNamespace string
		threshold             string
		expectedTriggerCount  int
		expectedType          string
		expectedName          string
		expectedMetricType    autoscalingv2.MetricTargetType
		expectedAuthRefName   string
		expectedAuthRefKind   string
	}{
		{
			name:                  "basic trigger creation",
			inferenceSetName:      "test-inference-set",
			inferenceSetNamespace: "test-namespace",
			threshold:             "10",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
		{
			name:                  "different threshold value",
			inferenceSetName:      "another-inference-set",
			inferenceSetNamespace: "another-namespace",
			threshold:             "5",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
		{
			name:                  "empty threshold",
			inferenceSetName:      "test-inference-set",
			inferenceSetNamespace: "test-namespace",
			threshold:             "",
			expectedTriggerCount:  1,
			expectedType:          "external",
			expectedName:          "keda-kaito-scaler",
			expectedMetricType:    autoscalingv2.AverageValueMetricType,
			expectedAuthRefName:   "keda-kaito-scaler-creds",
			expectedAuthRefKind:   "ClusterTriggerAuthentication",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			triggers := getDefaultKedaKaitoScalerTriggers(tt.inferenceSetName, tt.inferenceSetNamespace, tt.threshold)

			// Check trigger count
			assert.Equal(t, tt.expectedTriggerCount, len(triggers))

			if len(triggers) > 0 {
				trigger := triggers[0]

				// Check basic properties
				assert.Equal(t, tt.expectedType, trigger.Type)
				assert.Equal(t, tt.expectedName, trigger.Name)
				assert.Equal(t, tt.expectedMetricType, trigger.MetricType)

				// Check authentication reference
				assert.NotNil(t, trigger.AuthenticationRef)
				assert.Equal(t, tt.expectedAuthRefName, trigger.AuthenticationRef.Name)
				assert.Equal(t, tt.expectedAuthRefKind, trigger.AuthenticationRef.Kind)

				// Check metadata
				assert.NotNil(t, trigger.Metadata)
				assert.Equal(t, "keda-kaito-scaler", trigger.Metadata["scalerName"])
				assert.Equal(t, tt.threshold, trigger.Metadata["threshold"])
				assert.Equal(t, tt.inferenceSetName, trigger.Metadata[scaler.InferenceSetNameInMetadata])
				assert.Equal(t, tt.inferenceSetNamespace, trigger.Metadata[scaler.InferenceSetNamespaceInMetadata])
				assert.Equal(t, fmt.Sprintf("keda-kaito-scaler-svc.%s.svc.cluster.local:%d", tt.inferenceSetNamespace, 10450), trigger.Metadata[scaler.ScalerAddressInMetadata])
				assert.Equal(t, "vllm:num_requests_waiting", trigger.Metadata[scaler.MetricNameInMetadata])
				assert.Equal(t, "http", trigger.Metadata[scaler.MetricProtocolInMetadata])
				assert.Equal(t, "80", trigger.Metadata[scaler.MetricPortInMetadata])
				assert.Equal(t, "/metrics", trigger.Metadata[scaler.MetricPathInMetadata])
				assert.Equal(t, "5s", trigger.Metadata[scaler.ScrapeTimeoutInMetadata])
			}
		})
	}
}

func TestGetDefaultKedaKaitoScalerTriggers_MetadataKeys(t *testing.T) {
	inferenceSetName := "test-inference-set"
	inferenceSetNamespace := "test-namespace"
	threshold := "10"

	triggers := getDefaultKedaKaitoScalerTriggers(inferenceSetName, inferenceSetNamespace, threshold)

	assert.Equal(t, 1, len(triggers))
	trigger := triggers[0]

	// Verify all expected metadata keys are present
	expectedKeys := []string{
		"scalerName",
		"threshold",
		scaler.InferenceSetNameInMetadata,
		scaler.InferenceSetNamespaceInMetadata,
		scaler.ScalerAddressInMetadata,
		scaler.MetricNameInMetadata,
		scaler.MetricProtocolInMetadata,
		scaler.MetricPortInMetadata,
		scaler.MetricPathInMetadata,
		scaler.ScrapeTimeoutInMetadata,
	}

	for _, key := range expectedKeys {
		assert.Contains(t, trigger.Metadata, key, "Expected metadata key %s to be present", key)
	}

	// Verify metadata count matches expected keys
	assert.Equal(t, len(expectedKeys), len(trigger.Metadata))
}
