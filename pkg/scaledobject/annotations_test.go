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
	"strings"
	"testing"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
)

func TestTuningAnnotationAliases(t *testing.T) {
	tests := []struct{ canonical, legacy, value, different string }{
		{"scaledobject.kaito.sh/combinePolicy", constants.AnnotationKeyCombinePolicy, "AND", "and"},
		{"scaledobject.kaito.sh/scaleUpStabilizationWindow", constants.AnnotationKeyScaleUpStabilizationWindow, "45", "045"},
		{"scaledobject.kaito.sh/scaleDownStabilizationWindow", constants.AnnotationKeyScaleDownStabilizationWindow, "120", "121"},
		{"scaledobject.kaito.sh/scaleUpCooldown", constants.AnnotationKeyScaleUpCooldown, "120", "121"},
		{"scaledobject.kaito.sh/scaleDownCooldown", constants.AnnotationKeyScaleDownCooldown, "240", "241"},
		{"scaledobject.kaito.sh/scaleUpTolerance", constants.AnnotationKeyScaleUpTolerance, "0.2", "0.20"},
		{"scaledobject.kaito.sh/scaleDownTolerance", constants.AnnotationKeyScaleDownTolerance, "0.3", "0.30"},
		{"scaledobject.kaito.sh/cooldownPeriod", constants.AnnotationKeyCooldownPeriod, "600", "601"},
		{"scaledobject.kaito.sh/initialCooldownPeriod", constants.AnnotationKeyInitialCooldownPeriod, "300", "301"},
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
	ann["scaledobject.kaito.sh/combinePolicy"] = "AND"
	ann["scaledobject.kaito.sh/scaleUpStabilizationWindow"] = "45"
	ann[constants.AnnotationKeyScaleDownStabilizationWindow] = "120"
	ann["scaledobject.kaito.sh/scaleUpCooldown"] = "60"
	ann[constants.AnnotationKeyScaleDownCooldown] = "90"
	ann["scaledobject.kaito.sh/scaleUpTolerance"] = "0.2"
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

func TestTuningAnnotationCaseInsensitiveNames(t *testing.T) {
	for _, key := range []string{"scaleupStabilizationWindow", "ScaleUpStabilizationWindow", "SCALEUPSTABILIZATIONWINDOW"} {
		ann := metricsAnnotations()
		ann["scaledobject.kaito.sh/"+key] = "45"
		cfg, err := parseMetricsConfig(ann, 1)
		require.NoError(t, err)
		assert.Equal(t, int32(45), cfg.scaleUpStabilizationWindow)
	}
	assert.Error(t, ValidateConfig(nil, 1))
}

func TestCamelCaseScaleToZeroRequiresZeroMinimum(t *testing.T) {
	for _, key := range []string{"scaledobject.kaito.sh/cooldownPeriod", "scaledobject.kaito.sh/initialCooldownPeriod"} {
		for _, value := range []string{"", "0", "600"} {
			ann := metricsAnnotations()
			ann[key] = value
			assert.ErrorContains(t, ValidateConfig(ann, 1), "requires")
			_, err := (Builder{}).BuildDesired(&kaitov1beta1.InferenceSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}, 1, 5)
			assert.ErrorContains(t, err, "requires")
		}
	}
}

func TestNormalizeAnnotations(t *testing.T) {
	ann := map[string]string{
		"scaledobject.kaito.sh/NewTuningKey-123":       "Keep This VALUE",
		"scaledobject.kaito.sh/AUTO-PROVISION":         "true",
		"scaledobject.kaito.sh/MiN-RePlIcAs":           "0",
		"scaledobject.kaito.sh/MeTrIcS":                "- {name: MixedCaseMetric, type: gauge, upthreshold: 10, downthreshold: 2}\n",
		"other.example/NewTuningKey-123":               "Other VALUE",
		"other.example/newtuningkey-123":               "different",
		"ScaledObject.kaito.sh/NewTuningKey-123":       "wrong prefix",
		"scaledobject.kaito.sh.extra/NewTuningKey-123": "similar prefix",
		"NewTuningKey-123":                             "no prefix",
	}
	before := maps.Clone(ann)
	got, err := NormalizeAnnotations(ann)
	require.NoError(t, err)
	expected := maps.Clone(ann)
	for _, key := range []string{"NewTuningKey-123", "AUTO-PROVISION", "MiN-RePlIcAs", "MeTrIcS"} {
		delete(expected, constants.AnnotationPrefix+key)
		expected[constants.AnnotationPrefix+strings.ToLower(key)] = ann[constants.AnnotationPrefix+key]
	}
	assert.Equal(t, expected, got)
	assert.Equal(t, before, ann)
	again, err := NormalizeAnnotations(got)
	require.NoError(t, err)
	assert.Equal(t, got, again, "normalization must be idempotent")
	got["NewTuningKey-123"] = "changed"
	assert.Equal(t, before, ann, "the returned map must not alias the input")
	got, err = NormalizeAnnotations(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestNormalizeAnnotationCollisions(t *testing.T) {
	for _, name := range []string{"auto-provision", "min-replicas", "max-replicas", "metrics", "future-key"} {
		t.Run(name, func(t *testing.T) {
			first := constants.AnnotationPrefix + strings.ToUpper(name)
			second := constants.AnnotationPrefix + name
			for _, values := range [][2]string{{"same", "same"}, {"", ""}, {"60", "060"}, {"AND", "and"}, {"", "value"}, {"value", ""}} {
				ann := map[string]string{first: values[0], second: values[1]}
				before := maps.Clone(ann)
				got, err := NormalizeAnnotations(ann)
				if values[0] == values[1] {
					require.NoError(t, err)
					assert.Equal(t, map[string]string{second: values[0]}, got)
				} else {
					require.ErrorContains(t, err, "conflicting annotations")
					assert.ErrorContains(t, err, first)
					assert.ErrorContains(t, err, second)
					assert.Nil(t, got)
				}
				assert.Equal(t, before, ann)
			}
		})
	}
}

func TestNormalizeAnnotationConflictOrder(t *testing.T) {
	ann := map[string]string{
		"scaledobject.kaito.sh/FUTURE": "same",
		"scaledobject.kaito.sh/Future": "same",
		"scaledobject.kaito.sh/future": "different",
		"scaledobject.kaito.sh/ZKEY":   "first",
		"scaledobject.kaito.sh/zkey":   "second",
	}
	for range 20 {
		_, err := NormalizeAnnotations(ann)
		assert.EqualError(t, err, `conflicting annotations scaledobject.kaito.sh/FUTURE="same" and scaledobject.kaito.sh/future="different": values must match exactly`)
	}
}

func TestMetricsAnnotationCaseInsensitiveName(t *testing.T) {
	for _, key := range []string{"METRICS", "mEtRiCs"} {
		ann := metricsAnnotations()
		expected, err := parseMetricsConfig(ann, 1)
		require.NoError(t, err)
		ann[constants.AnnotationPrefix+key] = ann[constants.AnnotationKeyMetrics]
		delete(ann, constants.AnnotationKeyMetrics)
		got, err := parseMetricsConfig(ann, 1)
		require.NoError(t, err)
		assert.Equal(t, expected, got)
	}
}
