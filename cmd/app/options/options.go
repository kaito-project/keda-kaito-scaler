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
package options

import (
	"time"

	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/config/options"
)

type KedaKaitoScalerOptions struct {
	Version                bool
	GrpcPort               int
	WebhookPort            int
	MetricsPort            int
	HealthProbePort        int
	EnableProfiling        bool
	LeaderElection         componentbaseconfig.LeaderElectionConfiguration
	KubeClientQPS          int
	KubeClientBurst        int
	WorkingNamespace       string
	WebhookSecretName      string
	ScalerClientSecretName string
	ScalerServerSecretName string
	ScalerServiceName      string
	ExpirationDuration     time.Duration
}

func NewKedaKaitoScalerOptions() *KedaKaitoScalerOptions {
	return &KedaKaitoScalerOptions{
		Version:         false,
		GrpcPort:        10450,
		WebhookPort:     10451,
		MetricsPort:     10452,
		HealthProbePort: 10453,
		EnableProfiling: true,
		LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
			LeaderElect:       true,
			ResourceLock:      resourcelock.LeasesResourceLock,
			ResourceName:      "keda-kaito-scaler",
			ResourceNamespace: "kaito-workspace",
		},
		KubeClientQPS:          50,
		KubeClientBurst:        100,
		WorkingNamespace:       "kaito-workspace",
		WebhookSecretName:      "keda-kaito-scaler-webhook-certs",
		ScalerClientSecretName: "keda-kaito-scaler-client-certs",
		ScalerServerSecretName: "keda-kaito-scaler-server-certs",
		ScalerServiceName:      "keda-kaito-scaler-svc",
		ExpirationDuration:     3 * 364 * 24 * time.Hour, // 3 years
	}
}

func (o *KedaKaitoScalerOptions) AddFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&o.Version, "version", o.Version, "print the version information, and then exit")
	fs.IntVar(&o.GrpcPort, "grpc-port", o.GrpcPort, "the port the grpc endpoint binds to for serving grpc requests.")
	fs.IntVar(&o.WebhookPort, "webhook-port", o.WebhookPort, "the port the webhook endpoint binds to for validating and mutating resources.")
	fs.IntVar(&o.MetricsPort, "metrics-port", o.MetricsPort, "the port the metric endpoint binds to for serving metrics about keda-kaito-scaler.")
	fs.IntVar(&o.HealthProbePort, "health-probe-port", o.HealthProbePort, "the port the health probe endpoint binds to for serving livness check.")
	fs.BoolVar(&o.EnableProfiling, "enable-profiling", o.EnableProfiling, "enable the profiling on the metric endpoint.")
	options.BindLeaderElectionFlags(&o.LeaderElection, fs)
	fs.IntVar(&o.KubeClientQPS, "kube-client-qps", o.KubeClientQPS, "the rate of qps to kube-apiserver.")
	fs.IntVar(&o.KubeClientBurst, "kube-client-burst", o.KubeClientBurst, "the max allowed burst of queries to the kube-apiserver.")
	fs.StringVar(&o.WorkingNamespace, "working-namespace", o.WorkingNamespace, "the namespace where the keda-kaito-scaler is working.")
	fs.StringVar(&o.WebhookSecretName, "webhook-secret-name", o.WebhookSecretName, "the secret which used for storing certificates for keda-kaito-scaler webhook")
	fs.StringVar(&o.ScalerClientSecretName, "scaler-client-secret-name", o.ScalerClientSecretName, "the secret which used for storing certificates for keda-kaito-scaler client")
	fs.StringVar(&o.ScalerServerSecretName, "scaler-server-secret-name", o.ScalerServerSecretName, "the secret which used for storing certificates for keda-kaito-scaler server")
	fs.StringVar(&o.ScalerServiceName, "scaler-service-name", o.ScalerServiceName, "the service which used for accessing keda-kaito-scaler")
	fs.DurationVar(&o.ExpirationDuration, "cert-duration", o.ExpirationDuration, "the expiration duration of webhook server certificates")
}
