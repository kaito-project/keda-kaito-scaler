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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	runtimewebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kaito-project/keda-kaito-scaler/cmd/app/options"
	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers"
	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/util"
	"github.com/kaito-project/keda-kaito-scaler/pkg/injections"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/profile"
	"github.com/kaito-project/keda-kaito-scaler/pkg/webhooks"
)

const (
	KedaKaitoScaler = "keda-kaito-scaler"
)

func NewKedaKaitoScalerCommand() *cobra.Command {
	opts := options.NewKedaKaitoScalerOptions()

	cmd := &cobra.Command{
		Use:     KedaKaitoScaler,
		Version: injections.VersionInfo(),
		Run: func(cmd *cobra.Command, args []string) {
			cliflag.PrintFlags(cmd.Flags())

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

	//logging
	logger := klog.NewKlogr()
	log.SetLogger(logger)

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
	cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(opts.KubeClientQPS), opts.KubeClientBurst)
	cfg.UserAgent = KedaKaitoScaler

	// prepare webhook server secret lister
	secretLister, err := prepareResourcesLister(ctx, cfg, opts.WorkingNamespace)
	if err != nil {
		return err
	}

	// controller-runtime manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Metrics:                       metricsServerOpts,
		HealthProbeBindAddress:        fmt.Sprintf(":%d", opts.HealthProbePort),
		LeaderElection:                opts.LeaderElection.LeaderElect,
		LeaderElectionID:              opts.LeaderElection.ResourceName,
		LeaderElectionNamespace:       opts.WorkingNamespace,
		LeaderElectionResourceLock:    opts.LeaderElection.ResourceLock,
		LeaderElectionReleaseOnCancel: true,
		WebhookServer: runtimewebhook.NewServer(runtimewebhook.Options{
			Port: opts.WebhookPort,
			TLSOpts: []func(*tls.Config){
				func(cfg *tls.Config) {
					cfg.MinVersion = tls.VersionTLS13
					cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
						return loadCertificateFromSecret(secretLister, opts.WorkingNamespace, opts.WebhookSecretName)
					}
				},
			},
		}),
		Logger: logger,
		Cache: runtimecache.Options{
			DefaultTransform: runtimecache.TransformStripManagedFields(),
		},
	})
	if err != nil {
		klog.Errorf("failed to new manager, %v", err)
		return err
	}

	// endpoints for liveness and readiness
	lo.Must0(mgr.AddHealthzCheck("healthz", healthz.Ping))
	lo.Must0(mgr.AddReadyzCheck("readyz", healthz.Ping))

	clk := clock.RealClock{}

	// add cert controller for webhook certificates
	if err := rotator.AddRotator(mgr, &rotator.CertRotator{
		SecretKey: types.NamespacedName{
			Name:      opts.WebhookSecretName,
			Namespace: opts.WorkingNamespace,
		},
		CaCertDuration:        10 * 365 * 24 * time.Hour, // 10 years for ca certificates
		ServerCertDuration:    opts.ExpirationDuration,
		RequireLeaderElection: opts.LeaderElection.LeaderElect,
		Webhooks: []rotator.WebhookInfo{
			{
				Name: "keda-kaito-scaler-mutating-webhook-configuration",
				Type: rotator.Mutating,
			},
			{
				Name: "keda-kaito-scaler-validating-webhook-configuration",
				Type: rotator.Validating,
			},
		},
		FieldOwner:     "keda-kaito-scaler",
		CAName:         "keda-kaito-scaler-ca",
		CAOrganization: "kaito-project",
		DNSName:        fmt.Sprintf("%s.%s.svc", opts.ScalerServiceName, opts.WorkingNamespace),
		CertName:       util.ServerCert,
		KeyName:        util.ServerKey,
	}); err != nil {
		klog.Errorf("failed to add cert controller for webhook certificates, %v", err)
		return err
	}

	// initialize controllers
	controllers := controllers.NewControllers(mgr, clk, opts)
	for _, c := range controllers {
		lo.Must0(c.Register(ctx, mgr))
	}

	// initialize webhooks
	webhooks := webhooks.NewWebhooks(mgr, clk)
	for _, c := range webhooks {
		lo.Must0(c.Register(ctx, mgr))
	}

	// start the manager
	go func() {
		lo.Must0(mgr.Start(ctx))
	}()
	// start grpc server
	lo.Must0(startKaitoScalerServer(ctx, opts.GrpcPort, secretLister, opts.WorkingNamespace, opts.ScalerSecretName, opts.RequireMutualTLS))
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

func startKaitoScalerServer(ctx context.Context, port int, secretLister corev1listers.SecretLister, namespace, secretName string, requireMutualTLS bool) error {
	logger := log.FromContext(ctx).WithName("grpc-server")
	addr := fmt.Sprintf("0.0.0.0:%d", port)

	// Wait for the secret to be ready
	logger.Info("waiting for secret to be ready", "secret", secretName, "namespace", namespace)
	if err := waitForSecret(ctx, secretLister, namespace, secretName); err != nil {
		return fmt.Errorf("failed to wait for secret: %w", err)
	}
	logger.Info("secret is ready, starting TLS gRPC server", "address", addr, "mutualTLS", requireMutualTLS)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	// Create TLS credentials with dynamic certificate loading and root CAs
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return loadCertificateFromSecret(secretLister, namespace, secretName)
		},
	}

	// Configure client authentication based on the requirement
	if requireMutualTLS {
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		logger.Info("TLS configuration: requiring mutual TLS authentication")
	} else {
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		logger.Info("TLS configuration: optional client certificate verification")
	}

	// Load root CAs from secret for client certificate verification
	if rootCAs, err := loadRootCAsFromSecret(secretLister, namespace, secretName); err != nil {
		logger.Info("failed to load root CAs, using system default", "error", err)
		return err
	} else if rootCAs != nil {
		tlsConfig.ClientCAs = rootCAs
		logger.Info("loaded root CAs from secret for TLS configuration")
	}

	creds := credentials.NewTLS(tlsConfig)
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	externalscaler.RegisterExternalScalerServer(grpcServer, scaler.NewKaitoScaler())

	go func() {
		<-ctx.Done()
		logger.Info("shutting down gRPC server gracefully")
		grpcServer.GracefulStop()
	}()

	logger.Info("TLS gRPC server is serving", "address", addr)
	return grpcServer.Serve(lis)
}

// waitForSecret waits until the secret exists and contains the required certificate data
func waitForSecret(ctx context.Context, secretLister corev1listers.SecretLister, namespace, secretName string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			secret, err := secretLister.Secrets(namespace).Get(secretName)
			if err != nil {
				continue // Secret doesn't exist yet, keep waiting
			}

			// Check if the secret contains the required certificate data
			if hasRequiredCertificateData(secret.Data) {
				return nil
			}
		}
	}
}

// hasRequiredCertificateData checks if the secret contains server.crt, server.key, and ca.crt
func hasRequiredCertificateData(data map[string][]byte) bool {
	serverCert, hasCert := data[util.ServerCert]
	serverKey, hasKey := data[util.ServerKey]
	caCert, hasCA := data[util.CACert]
	return hasCert && hasKey && hasCA && len(serverCert) > 0 && len(serverKey) > 0 && len(caCert) > 0
}

// loadCertificateFromSecret loads the TLS certificate from the secret
// If we return (nil, error), the client sees - 'tls: internal error'
// If we return (nil, nil) the client sees - 'tls: no certificates configured'
func loadCertificateFromSecret(secretLister corev1listers.SecretLister, namespace, secretName string) (*tls.Certificate, error) {
	secret, err := secretLister.Secrets(namespace).Get(secretName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilerr
		}
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
	}

	serverCert, ok := secret.Data[util.ServerCert]
	if !ok {
		return nil, fmt.Errorf("server certificate not found in secret %s/%s", namespace, secretName)
	}

	serverKey, ok := secret.Data[util.ServerKey]
	if !ok {
		return nil, fmt.Errorf("server key not found in secret %s/%s", namespace, secretName)
	}

	cert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create X509 key pair: %w", err)
	}

	return &cert, nil
}

// loadRootCAsFromSecret loads the root CA certificates from the secret
func loadRootCAsFromSecret(secretLister corev1listers.SecretLister, namespace, secretName string) (*x509.CertPool, error) {
	secret, err := secretLister.Secrets(namespace).Get(secretName)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
	}

	caCertData, ok := secret.Data[util.CACert]
	if !ok {
		return nil, fmt.Errorf("CA certificate not found in secret %s/%s", namespace, secretName)
	}

	if len(caCertData) == 0 {
		return nil, fmt.Errorf("CA certificate is empty in secret %s/%s", namespace, secretName)
	}

	// Create a new certificate pool and add the CA certificate
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertData) {
		return nil, fmt.Errorf("failed to parse CA certificate from secret %s/%s", namespace, secretName)
	}

	return caCertPool, nil
}
