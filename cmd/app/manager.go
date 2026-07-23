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

package app

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kaito-project/keda-kaito-scaler/cmd/app/options"
	"github.com/kaito-project/keda-kaito-scaler/pkg/aggregator"
	"github.com/kaito-project/keda-kaito-scaler/pkg/constants"
	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers"
	"github.com/kaito-project/keda-kaito-scaler/pkg/injections"
	"github.com/kaito-project/keda-kaito-scaler/pkg/metricsource"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/cert"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/profile"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/runnable"
)

const (
	KedaKaitoScaler = "keda-kaito-scaler"

	// Certificate and key fields for comprehensive TLS setup
	// CA Certificate - Root certificate authority for scaler communication
	CACert = "ca.crt"

	// Server Certificate and Key - Used by the external Kaito scaler GRPC server
	ServerCert = "server.crt"
	ServerKey  = "server.key"

	// Client Certificate and Key - Used by KEDA core to authenticate with external scaler
	ClientCert = "tls.crt" // Standard kubernetes.io/tls format
	ClientKey  = "tls.key" // Standard kubernetes.io/tls format

	CACertDuration       = 10 * 365 * 24 * time.Hour // 10 years
	CAName               = "keda-kaito-scaler-ca"
	CAOrganization       = "kaito-project"
	ControllerFieldOwner = KedaKaitoScaler

	ServerCertDir = "/tmp/keda-kaito-scaler-certs/server"
	ClientCertDir = "/tmp/keda-kaito-scaler-certs/client"
)

func init() {
	// controller-runtime manager use scheme.Scheme by default
	utilruntime.Must(kaitov1beta1.AddToScheme(scheme.Scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme.Scheme))
}

func NewKedaKaitoScalerCommand() *cobra.Command {
	opts := options.NewKedaKaitoScalerOptions()

	cmd := &cobra.Command{
		Use:     KedaKaitoScaler,
		Version: injections.VersionInfo(),
		Run: func(cmd *cobra.Command, args []string) {
			cliflag.PrintFlags(cmd.Flags())
			klog.V(2).Infof("version: %s", injections.VersionInfo())

			if err := Run(opts); err != nil {
				klog.Fatalf("run keda-kaito-scaler failed, %v", err)
			}
		},
	}

	globalflag.AddGlobalFlags(cmd.Flags(), cmd.Name())
	opts.AddFlags(cmd.Flags())

	return cmd
}

func Run(opts *options.KedaKaitoScalerOptions) error {
	ctx := ctrl.SetupSignalHandler()

	// Unify logging: route controller-runtime logs through klog so every
	// component (manager, runnables, cert rotator) shares the same pipeline.
	// ctrl.NewManager picks up this global logger automatically when
	// ctrl.Options.Logger is left unset.
	log.SetLogger(klog.NewKlogr())
	setupLog := ctrl.Log.WithName("setup")

	// metrics server options
	metricsServerOpts := metricsserver.Options{
		BindAddress:   fmt.Sprintf(":%d", opts.MetricsPort),
		ExtraHandlers: make(map[string]http.Handler),
	}

	if opts.EnableProfiling {
		for path, handler := range profile.PprofHandlers {
			metricsServerOpts.ExtraHandlers[path] = handler
		}
	}

	// prepare rest config
	cfg := ctrl.GetConfigOrDie()
	// controller-runtime builds the default token-bucket rate limiter from
	// rest.Config.QPS / Burst, replacing the now-deprecated
	// flowcontrol.NewTokenBucketRateLimiter helper.
	cfg.QPS = float32(opts.KubeClientQPS)
	cfg.Burst = opts.KubeClientBurst
	cfg.UserAgent = KedaKaitoScaler

	// Wait for all required CRDs (Kaito InferenceSet + KEDA ScaledObject and
	// ClusterTriggerAuthentication) to be installed in the cluster. This
	// decouples the install order from KEDA / Kaito: keda-kaito-scaler can
	// be deployed before or after either of them.
	if err := waitForRequiredCRDs(ctx, cfg); err != nil {
		setupLog.Error(err, "failed waiting for required CRDs")
		return err
	}

	// Create / update the ClusterTriggerAuthentication resource the scaler
	// relies on for mTLS credentials. Owning it in code (instead of in the
	// Helm chart) removes the chart-level ordering constraint with KEDA.
	if err := ensureClusterTriggerAuthentication(ctx, cfg, opts); err != nil {
		setupLog.Error(err, "failed to ensure ClusterTriggerAuthentication")
		return err
	}

	// prepare webhook server secret lister
	secretLister, err := prepareResourcesLister(ctx, cfg, opts.WorkingNamespace)
	if err != nil {
		setupLog.Error(err, "failed to prepare secret lister")
		return err
	}

	// controller-runtime manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Metrics:                       metricsServerOpts,
		LeaderElection:                opts.LeaderElection.LeaderElect,
		LeaderElectionID:              opts.LeaderElection.ResourceName,
		LeaderElectionNamespace:       opts.WorkingNamespace,
		LeaderElectionResourceLock:    opts.LeaderElection.ResourceLock,
		LeaderElectionReleaseOnCancel: true,
		Cache: runtimecache.Options{
			DefaultTransform: runtimecache.TransformStripManagedFields(),
		},
	})
	if err != nil {
		setupLog.Error(err, "failed to new manager")
		return err
	}

	// add certificate controllers
	serverCertReady := make(chan struct{})
	clientCertReady := make(chan struct{})
	if err := addCertificateControllers(mgr, opts, serverCertReady, clientCertReady); err != nil {
		setupLog.Error(err, "failed to add certificate controllers")
		return err
	}

	// initialize controllers
	controllers := controllers.NewControllers(mgr, opts.WorkingNamespace, opts.ScalerServiceName, opts.GrpcPort)
	for _, c := range controllers {
		lo.Must0(c.Register(ctx, mgr))
	}

	// The metric cache poller runs on every replica (no leader election) and
	// continuously scrapes each tracked InferenceSet into an in-memory window, so
	// any replica can serve KEDA's GetMetrics from a complete local window.
	metricCache := scaler.NewMetricCache(
		mgr.GetClient(),
		map[string]metricsource.MetricSource{
			metricsource.ModelPodSourceName: metricsource.NewModelPodSource(mgr.GetClient()),
		},
	)
	lo.Must0(mgr.Add(runnable.NoLeaderElection(metricCache)))

	// register the KEDA external scaler gRPC server (mTLS) as a manager Runnable.
	// The runnable waits for the mTLS certificates to be ready before it starts
	// listening, so the manager alone drives the full lifecycle.
	lo.Must0(mgr.Add(scaler.NewRunnable(scaler.ServerConfig{
		Port: opts.GrpcPort,
		Service: scaler.NewKaitoScaler(
			mgr.GetClient(),
			metricCache,
			map[string]aggregator.Aggregator{
				aggregator.SumAggregatorName:            aggregator.NewSumAggregator(),
				aggregator.ServiceAverageAggregatorName: aggregator.NewServiceAverageAggregator(),
				// The windowed-average aggregation is served by the metric cache
				// itself (it holds the rolling snapshot window).
				constants.AggregationWindowedAvg: metricCache,
			},
		),
		GetServerCertificate: cert.NewServerCertLoader(secretLister, opts.WorkingNamespace, opts.ScalerServerSecretName, ServerCert, ServerKey),
		LoadRootCAs:          cert.NewRootCAsLoader(secretLister, opts.WorkingNamespace, opts.ScalerClientSecretName, CACert),
		ServerCertReady:      serverCertReady,
		ClientCertReady:      clientCertReady,
	})))

	// Register a lightweight gRPC health server so kubelet / KEDA can probe
	// liveness and readiness via grpc.health.v1.Health on a dedicated port.
	// Readiness is gated on the mTLS material being available, so the Pod
	// only enters the Service endpoints once KEDA can actually reach us.
	health := scaler.NewHealthServer()
	health.AddReadinessCheck("server-cert", scaler.ChannelReadinessCheck(serverCertReady))
	health.AddReadinessCheck("client-cert", scaler.ChannelReadinessCheck(clientCertReady))
	health.AddReadinessCheck("manager-cache", func(ctx context.Context) error {
		// Bound the wait: kubelet gRPC probe has its own timeout, but we do
		// not want Check() to hang on cold start either. 500ms is plenty once
		// the cache has already synced — subsequent calls return immediately.
		syncCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if mgr.GetCache().WaitForCacheSync(syncCtx) {
			return nil
		}
		return errors.New("controller cache not synced")
	})
	lo.Must0(mgr.Add(scaler.HealthRunnable(opts.GrpcHealthPort, health)))

	setupLog.Info("starting manager")
	return mgr.Start(ctx)
}

// waitForRequiredCRDs polls the API server until all CustomResourceDefinitions
// this scaler relies on (Kaito InferenceSet, KEDA ScaledObject and
// ClusterTriggerAuthentication) are installed. Polling — instead of failing
// fast — lets keda-kaito-scaler be deployed in any order relative to KEDA
// and the Kaito CRDs.
func waitForRequiredCRDs(ctx context.Context, cfg *rest.Config) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	required := []schema.GroupVersionKind{
		kaitov1beta1.GroupVersion.WithKind("InferenceSet"),
		v1alpha1.GroupVersion.WithKind("ScaledObject"),
		v1alpha1.GroupVersion.WithKind("ClusterTriggerAuthentication"),
	}

	logger := ctrl.Log.WithName("crd-wait")
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(_ context.Context) (bool, error) {
		missing := make([]string, 0, len(required))
		for _, gvk := range required {
			found, err := crdRegistered(discoveryClient, gvk)
			if err != nil {
				logger.V(2).Info("failed to query CRD; will retry", "gvk", gvk.String(), "err", err.Error())
				return false, nil
			}
			if !found {
				missing = append(missing, gvk.String())
			}
		}
		if len(missing) == 0 {
			return true, nil
		}
		logger.Info("waiting for required CRDs to be installed", "missing", missing)
		return false, nil
	})
}

// crdRegistered returns true when the given GroupVersionKind is discoverable
// in the cluster (i.e. its CRD has been installed).
func crdRegistered(dc discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (bool, error) {
	resources, err := dc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind {
			return true, nil
		}
	}
	return false, nil
}

// ensureClusterTriggerAuthentication creates or updates the cluster-scoped
// ClusterTriggerAuthentication resource that KEDA uses to load the mTLS
// credentials when querying the external scaler.
func ensureClusterTriggerAuthentication(ctx context.Context, cfg *rest.Config, opts *options.KedaKaitoScalerOptions) error {
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client for ClusterTriggerAuthentication: %w", err)
	}

	cta := &v1alpha1.ClusterTriggerAuthentication{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterTriggerAuthName},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, c, cta, func() error {
		cta.Spec = v1alpha1.TriggerAuthenticationSpec{
			SecretTargetRef: []v1alpha1.AuthSecretTargetRef{
				{Parameter: "caCert", Name: opts.ScalerServerSecretName, Key: CACert},
				{Parameter: "tlsClientCert", Name: opts.ScalerClientSecretName, Key: ClientCert},
				{Parameter: "tlsClientKey", Name: opts.ScalerClientSecretName, Key: ClientKey},
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create/update ClusterTriggerAuthentication %q: %w", constants.ClusterTriggerAuthName, err)
	}
	ctrl.Log.WithName("cluster-trigger-auth").Info("ensured ClusterTriggerAuthentication", "name", constants.ClusterTriggerAuthName, "operation", op)
	return nil
}

func prepareResourcesLister(ctx context.Context, cfg *rest.Config, ns string) (corev1listers.SecretLister, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 24*time.Hour, informers.WithNamespace(ns))
	secretInformer := factory.Core().V1().Secrets().Informer()
	secretLister := factory.Core().V1().Secrets().Lister()

	go factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), secretInformer.HasSynced) {
		return nil, errors.New("failed to wait for syncing cache for webhook server secret")
	}

	return secretLister, nil
}

func addCertificateControllers(mgr manager.Manager, opts *options.KedaKaitoScalerOptions, serverCertReady, clientCertReady chan struct{}) error {
	log := mgr.GetLogger().WithName("cert-controllers")
	dnsNames := []string{
		fmt.Sprintf("%s.%s", opts.ScalerServiceName, opts.WorkingNamespace),
		fmt.Sprintf("%s.%s.svc", opts.ScalerServiceName, opts.WorkingNamespace),
		fmt.Sprintf("%s.%s.svc.cluster", opts.ScalerServiceName, opts.WorkingNamespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", opts.ScalerServiceName, opts.WorkingNamespace),
	}

	// add cert controller for managing scaler client certificates of keda-kaito-scaler
	if err := rotator.AddRotator(mgr, &rotator.CertRotator{
		ControllerName: "scaler-client-cert-rotator",
		SecretKey: types.NamespacedName{
			Name:      opts.ScalerClientSecretName,
			Namespace: opts.WorkingNamespace,
		},
		CaCertDuration:        CACertDuration,
		ServerCertDuration:    opts.ExpirationDuration,
		RequireLeaderElection: opts.LeaderElection.LeaderElect,
		ExtKeyUsages:          &[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		FieldOwner:            ControllerFieldOwner,
		CAName:                CAName,
		CAOrganization:        CAOrganization,
		CertDir:               ClientCertDir,
		IsReady:               clientCertReady,
	}); err != nil {
		log.Error(err, "failed to add cert controller for scaler client certificates")
		return err
	}

	// add cert controller for managing scaler server certificates of keda-kaito-scaler
	if err := rotator.AddRotator(mgr, &rotator.CertRotator{
		ControllerName: "scaler-server-cert-rotator",
		SecretKey: types.NamespacedName{
			Name:      opts.ScalerServerSecretName,
			Namespace: opts.WorkingNamespace,
		},
		CaCertDuration:        CACertDuration,
		ServerCertDuration:    opts.ExpirationDuration,
		RequireLeaderElection: opts.LeaderElection.LeaderElect,
		FieldOwner:            ControllerFieldOwner,
		CAName:                CAName,
		CAOrganization:        CAOrganization,
		ExtraDNSNames:         dnsNames,
		CertName:              ServerCert,
		KeyName:               ServerKey,
		CertDir:               ServerCertDir,
		IsReady:               serverCertReady,
	}); err != nil {
		log.Error(err, "failed to add cert controller for scaler server certificates")
		return err
	}

	return nil
}
