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

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kaito-project/keda-kaito-scaler/cmd/app/options"
	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers"
	"github.com/kaito-project/keda-kaito-scaler/pkg/injections"
	"github.com/kaito-project/keda-kaito-scaler/pkg/scaler"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/profile"
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
	utilruntime.Must(kaitov1alpha1.AddToScheme(scheme.Scheme))
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
		Logger:                        logger,
		Cache: runtimecache.Options{
			DefaultTransform: runtimecache.TransformStripManagedFields(),
		},
	})
	if err != nil {
		logger.Error(err, "failed to new manager")
		return err
	}

	// endpoints for liveness and readiness
	lo.Must0(mgr.AddHealthzCheck("healthz", healthz.Ping))
	lo.Must0(mgr.AddReadyzCheck("readyz", healthz.Ping))

	// add certificate controllers
	serverCertReady := make(chan struct{})
	clientCertReady := make(chan struct{})
	err = addCertificateControllers(mgr, opts, serverCertReady, clientCertReady)
	if err != nil {
		logger.Error(err, "failed to add certificate controllers")
		return err
	}

	// initialize controllers
	controllers := controllers.NewControllers(mgr, opts.WorkingNamespace)
	for _, c := range controllers {
		lo.Must0(c.Register(ctx, mgr))
	}

	// start the manager
	go func() {
		lo.Must0(mgr.Start(ctx))
	}()
	// start grpc server
	lo.Must0(startKaitoScalerServer(ctx, mgr.GetClient(), secretLister, opts, serverCertReady, clientCertReady))
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

func startKaitoScalerServer(ctx context.Context, c client.Client, secretLister corev1listers.SecretLister, opts *options.KedaKaitoScalerOptions, serverCertReady, clientCertReady chan struct{}) error {
	logger := log.FromContext(ctx).WithName("grpc-server")
	addr := fmt.Sprintf("0.0.0.0:%d", opts.GrpcPort)

	// Wait for server secret to be ready
	logger.Info("waiting for server secret to be ready", "secret", opts.ScalerServerSecretName, "namespace", opts.WorkingNamespace)
	<-serverCertReady

	logger.Info("waiting for client secret to be ready", "secret", opts.ScalerClientSecretName, "namespace", opts.WorkingNamespace)
	<-clientCertReady

	logger.Info("secret is ready, starting TLS gRPC server", "address", addr)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	// Create TLS credentials with dynamic certificate loading and root CAs
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return loadCertificateFromSecret(secretLister, opts.WorkingNamespace, opts.ScalerServerSecretName)
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
	}

	// Load root CAs from secret for client certificate verification
	if rootCAs, err := loadRootCAsFromSecret(secretLister, opts.WorkingNamespace, opts.ScalerClientSecretName); err != nil {
		logger.Info("failed to load root CAs, using system default", "error", err)
		return err
	} else if rootCAs != nil {
		tlsConfig.ClientCAs = rootCAs
		logger.Info("loaded root CAs from secret for TLS configuration")
	}

	creds := credentials.NewTLS(tlsConfig)
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	externalscaler.RegisterExternalScalerServer(grpcServer, scaler.NewKaitoScaler(c))

	go func() {
		<-ctx.Done()
		logger.Info("shutting down gRPC server gracefully")
		grpcServer.GracefulStop()
	}()

	logger.Info("TLS gRPC server is serving", "address", addr)
	return grpcServer.Serve(lis)
}

func addCertificateControllers(mgr manager.Manager, opts *options.KedaKaitoScalerOptions, serverCertReady, clientCertReady chan struct{}) error {
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
		klog.Errorf("failed to add cert controller for scaler client certificates, %v", err)
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
		klog.Errorf("failed to add cert controller for scaler server certificates, %v", err)
		return err
	}

	return nil
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

	serverCert, ok := secret.Data[ServerCert]
	if !ok {
		return nil, fmt.Errorf("server certificate not found in secret %s/%s", namespace, secretName)
	}

	serverKey, ok := secret.Data[ServerKey]
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

	caCertData, ok := secret.Data[CACert]
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
