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

// Package scaledobject owns the InferenceSet autoscaling annotation schema and
// builds the desired KEDA ScaledObject for each autoscaling mode. Each mode
// (single-metric or composite multi-metric) is expressed as a Provisioner, so
// the reconciler stays agnostic of how each mode assembles its ScaledObject and
// new modes can be added without touching the reconcile loop.
package scaledobject

import (
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
)

// Builder carries the scaler service coordinates needed to build ScaledObject
// triggers that point back at the external scaler, and provides the per-mode
// ScaledObject construction consumed by the Provisioner strategies.
type Builder struct {
	ScalerNamespace   string
	ScalerServiceName string
	ScalerGRPCPort    int
}

// Provisioner builds the desired ScaledObject for one autoscaling mode of an
// InferenceSet. Abstracting the single-metric and composite modes behind a
// common interface keeps the reconciler agnostic of how each mode assembles its
// ScaledObject and leaves room for future modes.
type Provisioner interface {
	// Mode returns a short human-readable name used for logging.
	Mode() string
	// InvalidConfigReason is the Kubernetes event reason emitted when
	// BuildDesired reports an invalid configuration.
	InvalidConfigReason() string
	// BuildDesired validates the InferenceSet annotations and returns the desired
	// ScaledObject, or an error when the configuration is invalid.
	BuildDesired(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int) (*v1alpha1.ScaledObject, error)
}

// ProvisionerFor selects the Provisioner for an InferenceSet based on its
// annotations: composite (multi-metric) mode when the indexed composite
// annotations are present, single-metric mode otherwise.
func ProvisionerFor(b Builder, annotations map[string]string) Provisioner {
	if IsCompositeMode(annotations) {
		return &compositeProvisioner{builder: b}
	}
	return &singleMetricProvisioner{builder: b}
}

// singleMetricProvisioner builds a ScaledObject driven by a single scraped
// metric compared against a threshold.
type singleMetricProvisioner struct{ builder Builder }

func (p *singleMetricProvisioner) Mode() string                { return "single-metric" }
func (p *singleMetricProvisioner) InvalidConfigReason() string { return "InvalidConfig" }

func (p *singleMetricProvisioner) BuildDesired(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int) (*v1alpha1.ScaledObject, error) {
	threshold := is.Annotations[AnnotationKeyThreshold]
	metricName := resolveMetricName(is.Annotations)
	return p.builder.buildScaledObject(is, minReplicas, maxReplicas, threshold, metricName), nil
}

// compositeProvisioner builds a ScaledObject that combines several metrics via a
// KEDA scalingModifiers formula.
type compositeProvisioner struct{ builder Builder }

func (p *compositeProvisioner) Mode() string                { return "composite" }
func (p *compositeProvisioner) InvalidConfigReason() string { return "InvalidCompositeConfig" }

func (p *compositeProvisioner) BuildDesired(is *kaitov1beta1.InferenceSet, minReplicas, maxReplicas int) (*v1alpha1.ScaledObject, error) {
	cc, err := parseCompositeConfig(is.Annotations)
	if err != nil {
		return nil, err
	}
	return p.builder.buildCompositeScaledObject(is, minReplicas, maxReplicas, cc), nil
}
