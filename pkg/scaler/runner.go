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

package scaler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kaito-project/keda-kaito-scaler/pkg/util/runnable"
)

// ServerConfig bundles everything the main scaler gRPC server needs in order
// to come up as a manager.Runnable.
type ServerConfig struct {
	// Port is the TCP port the TLS gRPC server listens on.
	Port int

	// Service is the KaitoScaler implementation registered to the server.
	Service *KaitoScaler

	// GetServerCertificate is invoked on every TLS handshake to fetch a fresh
	// server certificate (enabling rotation via a secret watcher).
	GetServerCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)

	// LoadRootCAs returns the x509 pool used to authenticate the KEDA client
	// (mTLS). It is invoked once at startup.
	LoadRootCAs func() (*x509.CertPool, error)

	// ServerCertReady / ClientCertReady are closed by the cert-controller once
	// each side of the mTLS material is available. The runnable waits for both
	// before it starts listening; ctx cancellation aborts the wait.
	ServerCertReady <-chan struct{}
	ClientCertReady <-chan struct{}
}

// NewRunnable builds a manager.Runnable that starts the TLS mTLS gRPC scaler
// server once certificates are available. The runnable never participates in
// leader election: every replica must serve gRPC so KEDA can reach it.
func NewRunnable(cfg ServerConfig) manager.Runnable {
	return runnable.NoLeaderElection(manager.RunnableFunc(func(ctx context.Context) error {
		// Wait for TLS material. A cert controller closes these channels
		// asynchronously once the secrets are populated.
		if err := waitReady(ctx, cfg.ServerCertReady, "server cert"); err != nil {
			return err
		}
		if err := waitReady(ctx, cfg.ClientCertReady, "client cert"); err != nil {
			return err
		}

		tlsConfig := &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: cfg.GetServerCertificate,
			ClientAuth:     tls.RequireAndVerifyClientCert,
		}

		if cfg.LoadRootCAs != nil {
			rootCAs, err := cfg.LoadRootCAs()
			if err != nil {
				return fmt.Errorf("failed to load client CA pool: %w", err)
			}
			if rootCAs != nil {
				tlsConfig.ClientCAs = rootCAs
				klog.V(2).Info("loaded root CAs from secret for TLS configuration")
			}
		}

		srv := grpc.NewServer(
			grpc.Creds(credentials.NewTLS(tlsConfig)),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				MaxConnectionIdle: 15 * time.Minute,
				Time:              30 * time.Second,
				Timeout:           10 * time.Second,
			}),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
			grpc.MaxConcurrentStreams(256),
			// Recovery first, so metrics / access log always observe the
			// translated error rather than being skipped by a panic.
			grpc.ChainUnaryInterceptor(
				recoveryUnaryInterceptor(),
				metricsUnaryInterceptor(),
			),
			grpc.ChainStreamInterceptor(
				recoveryStreamInterceptor(),
				metricsStreamInterceptor(),
			),
		)
		externalscaler.RegisterExternalScalerServer(srv, cfg.Service)

		return runnable.GRPCServer("scaler", srv, cfg.Port).Start(ctx)
	}))
}

// HealthRunnable builds a plaintext gRPC server exposing the standard
// grpc.health.v1.Health service on the given port, wrapped as a
// manager.Runnable so kubelet grpc probes can target it. The caller is
// expected to configure readiness checks on health before registering.
func HealthRunnable(port int, health *HealthServer) manager.Runnable {
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health)
	return runnable.NoLeaderElection(runnable.GRPCServer("health", srv, port))
}

// waitReady blocks until the given channel is closed or the context is
// cancelled.
func waitReady(ctx context.Context, ready <-chan struct{}, name string) error {
	select {
	case <-ready:
		klog.V(2).Infof("%s is ready", name)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cancelled while waiting for %s: %w", name, ctx.Err())
	}
}
