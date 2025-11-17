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

package scaler

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kaito-project/kaito/pkg/utils/inferenceset"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"go.uber.org/multierr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ScalerName = "keda-kaito-scaler"

	// Metadata keys
	ScalerNameKeyInMetadata         = "scalerName"
	InferenceSetNameInMetadata      = "inferenceSetName"
	InferenceSetNamespaceInMetadata = "inferenceSetNamespace"
	ScalerAddressInMetadata         = "scalerAddress"
	MetricNameInMetadata            = "metricName"
	ThresholdInMetadata             = "threshold"
	MetricProtocolInMetadata        = "metricProtocol"
	MetricPortInMetadata            = "metricPort"
	MetricPathInMetadata            = "metricPath"
	ScrapeTimeoutInMetadata         = "scrapeTimeout"
)

type ScalerConfig struct {
	InferenceSetName      string
	InferenceSetNamespace string
	MetricName            string
	MetricProtocol        string
	MetricPort            string
	MetricPath            string
	ScrapeTimeout         time.Duration
	Threshold             int64
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="kaito.sh",resources=inferencesets,verbs=get;list;watch
// +kubebuilder:rbac:groups="kaito.sh",resources=workspaces,verbs=list;watch

type kaitoScaler struct {
	kubeClient    client.Client
	httpTransport *http.Transport
	tlsTransport  *http.Transport
	externalscaler.UnimplementedExternalScalerServer
}

func NewKaitoScaler(kubeClient client.Client) *kaitoScaler {
	return &kaitoScaler{
		kubeClient: kubeClient,
		httpTransport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
		tlsTransport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

func (e *kaitoScaler) IsActive(ctx context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.IsActiveResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	// get the related workspace instance
	inferenceSet := &kaitov1alpha1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	// get InferenceSetConditionTypeInferenceStatus condition from inferenceSet.Status.Conditions
	condition := metav1.Condition{}
	if inferenceSet != nil {
		for i := range inferenceSet.Status.Conditions {
			if inferenceSet.Status.Conditions[i].Type == string(kaitov1alpha1.InferenceSetConditionTypeReady) {
				condition = inferenceSet.Status.Conditions[i]
				break
			}
		}
	}

	return &externalscaler.IsActiveResponse{
		Result: condition.Status == metav1.ConditionTrue,
	}, nil
}

func (e *kaitoScaler) StreamIsActive(sor *externalscaler.ScaledObjectRef, server externalscaler.ExternalScaler_StreamIsActiveServer) error {
	// do nothing for stream mode
	return nil
}

func (e *kaitoScaler) GetMetricSpec(_ context.Context, sor *externalscaler.ScaledObjectRef) (*externalscaler.GetMetricSpecResponse, error) {
	scalerConfig, err := parseScalerMetadata(sor, "")
	if err != nil {
		return nil, err
	}

	return &externalscaler.GetMetricSpecResponse{
		MetricSpecs: []*externalscaler.MetricSpec{{
			MetricName: scalerConfig.MetricName,
			TargetSize: scalerConfig.Threshold,
		}},
	}, nil
}

func (e *kaitoScaler) GetMetrics(ctx context.Context, gmr *externalscaler.GetMetricsRequest) (*externalscaler.GetMetricsResponse, error) {
	scalerConfig, err := parseScalerMetadata(gmr.ScaledObjectRef, gmr.MetricName)
	if err != nil {
		return nil, err
	}

	inferenceSet := &kaitov1alpha1.InferenceSet{}
	if err := e.kubeClient.Get(ctx, client.ObjectKey{
		Namespace: scalerConfig.InferenceSetNamespace,
		Name:      scalerConfig.InferenceSetName,
	}, inferenceSet); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get InferenceSet(%s) in Namespace(%s): %v", scalerConfig.InferenceSetName, scalerConfig.InferenceSetNamespace, err))
	}

	// get the related workspace instances
	workspaceList, err := inferenceset.ListWorkspaces(ctx, inferenceSet, e.kubeClient)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to list workspaces: %v", err))
	}
	klog.V(6).Infof("total workspaces found: %d", len(workspaceList.Items))

	pods := make([]corev1.Pod, 0)
	for i := range workspaceList.Items {
		workspace := &workspaceList.Items[i]
		// get related pods for workspace based on the label selector(kaitov1beta1.LabelWorkspaceName=<workspace.Name>)
		var workspacePods corev1.PodList
		if err := e.kubeClient.List(ctx, &workspacePods, client.InNamespace(scalerConfig.InferenceSetNamespace), client.MatchingLabels{
			kaitov1beta1.LabelWorkspaceName: workspace.Name,
		}); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to list pods: %v", err))
		}
		pods = append(pods, workspacePods.Items...)
	}
	klog.V(6).Infof("total pods found: %d", len(pods))

	// get the metrics for the ready pods and calculate the total metric value.
	// at the same time, please count the number of pods that metric can be collected.
	var totalMetricValue int64
	var podCount int
	metricErrs := make([]error, len(pods))
	for i, pod := range pods {
		if !isPodReady(&pod) {
			continue
		}
		metricVal, err := e.getPodMetric(ctx, &pod, scalerConfig)
		if err != nil {
			metricErrs[i] = err
			klog.Errorf("failed to get metric from pod %s/%s: %v", pod.Namespace, pod.Name, err)
			continue
		}
		totalMetricValue += metricVal
		podCount++
	}

	// if a metric cannot be resolved from a pod, the average value will be calculated using the following rules to prevent flapping:
	// in the scale-up direction: use 0 as the metric value for missing pods.
	// in the scale-down direction: use the metric threshold as the value for missing pods.
	if podCount == 0 {
		if err := multierr.Combine(metricErrs...); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get pod metrics: %v", err))
		}
		return nil, status.Error(codes.Internal, "no ready pods found for the workspace")
	} else if podCount != len(pods) {
		if totalMetricValue/int64(podCount) < scalerConfig.Threshold {
			// scale-down direction
			totalMetricValue += scalerConfig.Threshold * int64(len(pods)-podCount)
		} else {
			// scale-up direction
			totalMetricValue += 0 * int64(len(pods)-podCount)
		}
	}
	averageMetricValue := totalMetricValue / int64(len(pods))

	return &externalscaler.GetMetricsResponse{
		MetricValues: []*externalscaler.MetricValue{
			{
				MetricName:  scalerConfig.MetricName,
				MetricValue: averageMetricValue,
			},
		},
	}, nil
}

func parseScalerMetadata(sor *externalscaler.ScaledObjectRef, metricName string) (*ScalerConfig, error) {
	if scalerName, ok := sor.ScalerMetadata[ScalerNameKeyInMetadata]; !ok || scalerName != ScalerName {
		return nil, status.Error(codes.InvalidArgument, "scaler name must be keda-kaito-scaler")
	}

	inferenceSetName, ok := sor.ScalerMetadata[InferenceSetNameInMetadata]
	if !ok || inferenceSetName == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set name must be specified")
	}

	inferenceSetNamespace, ok := sor.ScalerMetadata[InferenceSetNamespaceInMetadata]
	if !ok || inferenceSetNamespace == "" {
		return nil, status.Error(codes.InvalidArgument, "inference set namespace must be specified")
	}

	if metricName == "" {
		name, ok := sor.ScalerMetadata[MetricNameInMetadata]
		if !ok || name == "" {
			return nil, status.Error(codes.InvalidArgument, "metric name must be specified")
		}
		metricName = name
	}

	metricProtocol, ok := sor.ScalerMetadata[MetricProtocolInMetadata]
	if !ok || metricProtocol == "" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be specified")
	} else if metricProtocol != "http" && metricProtocol != "https" {
		return nil, status.Error(codes.InvalidArgument, "metric protocol must be either http or https")
	}

	metricPort, ok := sor.ScalerMetadata[MetricPortInMetadata]
	if !ok || metricPort == "" {
		return nil, status.Error(codes.InvalidArgument, "metric port must be specified")
	}

	metricPath, ok := sor.ScalerMetadata[MetricPathInMetadata]
	if !ok || metricPath == "" {
		return nil, status.Error(codes.InvalidArgument, "metric path must be specified")
	}

	scrapeTimeoutStr, ok := sor.ScalerMetadata[ScrapeTimeoutInMetadata]
	if !ok || scrapeTimeoutStr == "" {
		return nil, status.Error(codes.InvalidArgument, "scrape timeout must be specified")
	}

	// convert scrapeTimeoutStr to time.Duration
	scrapeTimeout, err := time.ParseDuration(scrapeTimeoutStr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "scrape timeout must be a valid duration")
	}

	thresholdStr, ok := sor.ScalerMetadata[ThresholdInMetadata]
	if !ok || thresholdStr == "" {
		return nil, status.Error(codes.InvalidArgument, "threshold must be specified")
	}

	// covert threshold to int64 number
	threshold, err := strconv.ParseInt(thresholdStr, 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "threshold must be a valid integer")
	}

	return &ScalerConfig{
		InferenceSetName:      inferenceSetName,
		InferenceSetNamespace: inferenceSetNamespace,
		MetricName:            metricName,
		MetricProtocol:        metricProtocol,
		MetricPort:            metricPort,
		MetricPath:            metricPath,
		ScrapeTimeout:         scrapeTimeout,
		Threshold:             threshold,
	}, nil
}

func (e *kaitoScaler) getPodMetric(_ context.Context, pod *corev1.Pod, scalerConfig *ScalerConfig) (int64, error) {
	var transport *http.Transport
	if scalerConfig.MetricProtocol == "https" {
		transport = e.tlsTransport
	} else {
		transport = e.httpTransport
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   scalerConfig.ScrapeTimeout,
	}

	metricURL := fmt.Sprintf("%s://%s:%s%s", scalerConfig.MetricProtocol, pod.Status.PodIP, scalerConfig.MetricPort, scalerConfig.MetricPath)
	klog.V(6).Infof("scraping metrics from pod %s/%s: %s", pod.Namespace, pod.Name, metricURL)
	resp, err := httpClient.Get(metricURL)
	if err != nil {
		return 0, status.Error(codes.Internal, fmt.Sprintf("failed to get pod metrics: %v", err))
	}
	defer resp.Body.Close()
	klog.V(6).Infof("scraped metrics from pod %s/%s: %s", pod.Namespace, pod.Name, metricURL)

	// scan the response line by line, and read the metricName
	// format is `vllm:num_requests_waiting 50`
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, scalerConfig.MetricName) {
			// extract the metric value
			klog.V(2).Infof("found metric line from pod %s/%s: %s", pod.Namespace, pod.Name, line)
			parts := strings.Split(line, " ")
			if len(parts) == 2 {
				value, err := strconv.ParseFloat(parts[1], 64)
				if err == nil {
					return int64(value), nil
				}
			}
		}
	}

	return 0, status.Error(codes.Internal, fmt.Sprintf("failed to resolve metric(%s) from pod: %s/%s", scalerConfig.MetricName, pod.Namespace, pod.Name))
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
