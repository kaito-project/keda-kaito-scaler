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

package scaledobject

import (
	"maps"
	"testing"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
)

func TestTuningAnnotationAliases(t *testing.T) {
	tests := []struct{ canonical, legacy, value, different string }{
		{constants.AnnotationKeyCombinePolicyCamelCase, constants.AnnotationKeyCombinePolicy, "AND", "and"},
		{constants.AnnotationKeyScaleUpStabilizationWindowCamelCase, constants.AnnotationKeyScaleUpStabilizationWindow, "45", "045"},
		{constants.AnnotationKeyScaleDownStabilizationWindowCamelCase, constants.AnnotationKeyScaleDownStabilizationWindow, "120", "121"},
		{constants.AnnotationKeyScaleUpCooldownCamelCase, constants.AnnotationKeyScaleUpCooldown, "120", "121"},
		{constants.AnnotationKeyScaleDownCooldownCamelCase, constants.AnnotationKeyScaleDownCooldown, "240", "241"},
		{constants.AnnotationKeyScaleUpToleranceCamelCase, constants.AnnotationKeyScaleUpTolerance, "0.2", "0.20"},
		{constants.AnnotationKeyScaleDownToleranceCamelCase, constants.AnnotationKeyScaleDownTolerance, "0.3", "0.30"},
		{constants.AnnotationKeyCooldownPeriodCamelCase, constants.AnnotationKeyCooldownPeriod, "600", "601"},
		{constants.AnnotationKeyInitialCooldownPeriodCamelCase, constants.AnnotationKeyInitialCooldownPeriod, "300", "301"},
	}
	for _, tt := range tests {
		t.Run(tt.canonical, func(t *testing.T) {
			build := func(ann map[string]string) {
				t.Helper()
				before := maps.Clone(ann)
				expectedAnn := scaleToZeroAnnotations()
				expectedAnn[tt.legacy] = ann[tt.canonical]
				if _, ok := ann[tt.canonical]; !ok {
					expectedAnn[tt.legacy] = ann[tt.legacy]
				}
				is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", Annotations: expectedAnn}}
				expected, err := (Builder{}).BuildDesired(is, 0, 5)
				require.NoError(t, err)
				is.Annotations = ann
				got, err := (Builder{}).BuildDesired(is, 0, 5)
				require.NoError(t, err)
				assert.Equal(t, expected.Spec, got.Spec)
				assert.NoError(t, ValidateConfig(ann, 0))
				assert.Equal(t, before, ann, "callers' annotations must not be mutated")
			}
			for _, form := range []string{"canonical", "legacy", "both", "canonical empty", "legacy empty", "both empty"} {
				t.Run(form, func(t *testing.T) {
					ann := scaleToZeroAnnotations()
					switch form {
					case "canonical":
						ann[tt.canonical] = tt.value
					case "legacy":
						ann[tt.legacy] = tt.value
					case "both":
						ann[tt.canonical], ann[tt.legacy] = tt.value, tt.value
					case "canonical empty":
						ann[tt.canonical] = ""
					case "legacy empty":
						ann[tt.legacy] = ""
					case "both empty":
						ann[tt.canonical], ann[tt.legacy] = "", ""
					}
					build(ann)
				})
			}
			for _, pair := range [][2]string{{tt.value, tt.different}, {"", tt.value}, {tt.value, ""}} {
				ann := scaleToZeroAnnotations()
				ann[tt.canonical], ann[tt.legacy] = pair[0], pair[1]
				before := maps.Clone(ann)
				err := ValidateConfig(ann, 0)
				require.ErrorContains(t, err, "conflicting annotations")
				assert.ErrorContains(t, err, tt.canonical)
				assert.ErrorContains(t, err, tt.legacy)
				is := &kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
				got, err := (Builder{}).BuildDesired(is, 0, 5)
				assert.Nil(t, got)
				assert.ErrorContains(t, err, "conflicting annotations")
				assert.Equal(t, before, ann)
			}
			t.Run("invalid canonical value", func(t *testing.T) {
				ann := scaleToZeroAnnotations()
				ann[tt.canonical] = "invalid"
				assert.Error(t, ValidateConfig(ann, 0))
				_, err := (Builder{}).BuildDesired(&kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}, 0, 5)
				assert.Error(t, err)
			})
		})
	}
}

func TestTuningAnnotationMixedSpellings(t *testing.T) {
	ann := metricsAnnotations()
	ann[constants.AnnotationKeyCombinePolicyCamelCase] = "AND"
	ann[constants.AnnotationKeyScaleUpStabilizationWindowCamelCase] = "45"
	ann[constants.AnnotationKeyScaleDownStabilizationWindow] = "120"
	ann[constants.AnnotationKeyScaleUpCooldownCamelCase] = "60"
	ann[constants.AnnotationKeyScaleDownCooldown] = "90"
	ann[constants.AnnotationKeyScaleUpToleranceCamelCase] = "0.2"
	ann[constants.AnnotationKeyScaleDownTolerance] = "0.3"
	cfg, err := parseMetricsConfig(ann, 1)
	require.NoError(t, err)
	assert.Equal(t, int32(45), cfg.scaleUpStabilizationWindow)
	assert.Equal(t, int32(120), cfg.scaleDownStabilizationWindow)
	assert.Equal(t, int32(60), cfg.scaleUpCooldown)
	assert.Equal(t, int32(90), cfg.scaleDownCooldown)
	assert.InDelta(t, 0.2, cfg.scaleUpTolerance.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 0.3, cfg.scaleDownTolerance.AsApproximateFloat64(), 1e-9)
}

func TestTuningAnnotationExactSpellings(t *testing.T) {
	for _, key := range []string{"scaleupStabilizationWindow", "ScaleUpStabilizationWindow", "SCALEUPSTABILIZATIONWINDOW"} {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/"+key] = "45"
		cfg, err := parseMetricsConfig(ann, 1)
		require.NoError(t, err)
		assert.Equal(t, int32(defaultScaleUpStabilizationWindow), cfg.scaleUpStabilizationWindow)
	}
	assert.Error(t, ValidateConfig(nil, 1))
}

func TestCamelCaseScaleToZeroRequiresZeroMinimum(t *testing.T) {
	for _, key := range []string{constants.AnnotationKeyCooldownPeriodCamelCase, constants.AnnotationKeyInitialCooldownPeriodCamelCase} {
		for _, value := range []string{"", "0", "600"} {
			ann := metricsAnnotations()
			ann[key] = value
			assert.ErrorContains(t, ValidateConfig(ann, 1), "requires")
			_, err := (Builder{}).BuildDesired(&kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}, 1, 5)
			assert.ErrorContains(t, err, "requires")
		}
	}
}
