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
	"context"
	"fmt"

	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
)

type ScaledObjectWebhook struct {
	WorkingNamespace string
}

func NewWebhook(ns string) *ScaledObjectWebhook {
	return &ScaledObjectWebhook{
		WorkingNamespace: ns,
	}
}

func (w *ScaledObjectWebhook) Default(ctx context.Context, obj runtime.Object) error {
	scaledObject, ok := obj.(*v1alpha1.ScaledObject)
	if !ok {
		return nil
	}

	// only handle scaledObject with trigger type is external and scalerName is keda-kaito-scaler
	hasKaitoScaler := false
	kaitoScalerIndex := 0
	for i := range scaledObject.Spec.Triggers {
		trigger := scaledObject.Spec.Triggers[i]
		if trigger.Type == "external" && trigger.Name == scaler.ScalerName {
			hasKaitoScaler = true
			kaitoScalerIndex = i
			break
		}
	}

	if !hasKaitoScaler {
		return nil
	}

	if scaledObject.Spec.MinReplicaCount == nil {
		scaledObject.Spec.MinReplicaCount = ptr.To(int32(1))
	}

	configureDefaultAdvancedHorizontalPodAutoscalerConfig(scaledObject)

	configureKedaKaitoScalerTrigger(scaledObject, kaitoScalerIndex, w.WorkingNamespace)

	return nil
}

func configureDefaultAdvancedHorizontalPodAutoscalerConfig(obj *v1alpha1.ScaledObject) {
	if obj.Spec.Advanced == nil {
		obj.Spec.Advanced = &v1alpha1.AdvancedConfig{
			HorizontalPodAutoscalerConfig: &v1alpha1.HorizontalPodAutoscalerConfig{
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{},
			},
		}
	}

	advanced := obj.Spec.Advanced
	if advanced.HorizontalPodAutoscalerConfig == nil {
		advanced.HorizontalPodAutoscalerConfig = &v1alpha1.HorizontalPodAutoscalerConfig{
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{},
		}
	}

	if advanced.HorizontalPodAutoscalerConfig.Behavior == nil {
		advanced.HorizontalPodAutoscalerConfig.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
	}

	if advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleUp == nil {
		upTolerance := resource.MustParse("0.1")
		advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleUp = &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(60)),
			SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
			Policies: []autoscalingv2.HPAScalingPolicy{
				{
					Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
					Value:         1,
					PeriodSeconds: 300,
				},
			},
			Tolerance: &upTolerance,
		}
	}

	if advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleDown == nil {
		downTolerance := resource.MustParse("0.5")
		advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleDown = &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(int32(300)),
			SelectPolicy:               ptr.To(autoscalingv2.MaxChangePolicySelect),
			Policies: []autoscalingv2.HPAScalingPolicy{
				{
					Type:          autoscalingv2.HPAScalingPolicyType(autoscalingv2.PodsScalingPolicy),
					Value:         1,
					PeriodSeconds: 600,
				},
			},
			Tolerance: &downTolerance,
		}
	}
}

func configureKedaKaitoScalerTrigger(obj *v1alpha1.ScaledObject, triggerIndex int, workingNamespace string) {
	kaitoScalerTrigger := &obj.Spec.Triggers[triggerIndex]
	if kaitoScalerTrigger.Metadata == nil {
		kaitoScalerTrigger.Metadata = make(map[string]string)
	}

	if inferenceSetName, ok := kaitoScalerTrigger.Metadata[scaler.InferenceSetNameInMetadata]; !ok || len(inferenceSetName) == 0 {
		kaitoScalerTrigger.Metadata[scaler.InferenceSetNameInMetadata] = obj.Spec.ScaleTargetRef.Name
	}

	if inferenceSetNamespace, ok := kaitoScalerTrigger.Metadata[scaler.InferenceSetNamespaceInMetadata]; !ok || len(inferenceSetNamespace) == 0 {
		kaitoScalerTrigger.Metadata[scaler.InferenceSetNamespaceInMetadata] = obj.Namespace
	}

	if scalerAddress, ok := kaitoScalerTrigger.Metadata[scaler.ScalerAddressInMetadata]; !ok || len(scalerAddress) == 0 {
		kaitoScalerTrigger.Metadata[scaler.ScalerAddressInMetadata] = fmt.Sprintf("keda-kaito-scaler-svc.%s.svc.cluster.local:%d", workingNamespace, 10450)
	}

	if name, ok := kaitoScalerTrigger.Metadata[scaler.MetricNameInMetadata]; !ok || len(name) == 0 {
		kaitoScalerTrigger.Metadata[scaler.MetricNameInMetadata] = "vllm:num_requests_waiting"
	}

	if protocol, ok := kaitoScalerTrigger.Metadata[scaler.MetricProtocolInMetadata]; !ok || len(protocol) == 0 {
		kaitoScalerTrigger.Metadata[scaler.MetricProtocolInMetadata] = "https"
	}

	if port, ok := kaitoScalerTrigger.Metadata[scaler.MetricPortInMetadata]; !ok || len(port) == 0 {
		kaitoScalerTrigger.Metadata[scaler.MetricPortInMetadata] = "5000"
	}

	if path, ok := kaitoScalerTrigger.Metadata[scaler.MetricPathInMetadata]; !ok || len(path) == 0 {
		kaitoScalerTrigger.Metadata[scaler.MetricPathInMetadata] = "/metrics"
	}

	if timeout, ok := kaitoScalerTrigger.Metadata[scaler.ScrapeTimeoutInMetadata]; !ok || len(timeout) == 0 {
		kaitoScalerTrigger.Metadata[scaler.ScrapeTimeoutInMetadata] = "5s"
	}

	if kaitoScalerTrigger.AuthenticationRef == nil {
		kaitoScalerTrigger.AuthenticationRef = &v1alpha1.AuthenticationRef{
			Name: "keda-kaito-scaler-creds",
			Kind: "ClusterTriggerAuthentication",
		}
	}

	if len(kaitoScalerTrigger.MetricType) == 0 {
		kaitoScalerTrigger.MetricType = autoscalingv2.AverageValueMetricType
	}
}

// +kubebuilder:webhook:path=/mutate-keda-sh-v1alpha1-scaledobject,mutating=true,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1,groups=keda.sh,resources=scaledobjects,verbs=create,versions=v1alpha1,name=mutating.scaledobjects.kaito.sh,serviceName=keda-kaito-scaler-svc,serviceNamespace=kaito-workspace

func (w *ScaledObjectWebhook) Register(_ context.Context, mgr manager.Manager) error {
	return controllerruntime.NewWebhookManagedBy(mgr).
		For(&v1alpha1.ScaledObject{}).
		WithDefaulter(w).
		Complete()
}
