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

// Package secret provides the secret controller responsible for managing TLS certificates
// required for secure GRPC communication between KEDA core and the external Kaito scaler.
//
// This controller ensures that certificates and secrets are automatically generated,
// distributed, and renewed without manual intervention.
//
// Certificate Structure:
//   - CA Certificate: Root certificate authority for the scaler communication
//   - Server Certificate: Used by the external Kaito scaler GRPC server
//   - Client Certificate: Used by KEDA core to authenticate with the external scaler
//   - DNS SANs: Includes all necessary service names and IPs for flexible deployment
//     (e.g., scaler service FQDN: kaito-scaler.keda.svc.cluster.local)
//
// The generated secret follows the kubernetes.io/tls format with additional fields:
//
//	apiVersion: v1
//	kind: Secret
//	metadata:
//	  name: keda-kaito-scaler-certs
//	  namespace: keda
//	type: kubernetes.io/tls
//	data:
//	  ca.crt: <base64-encoded-ca-certificate>
//	  tls.crt: <base64-encoded-client-certificate>
//	  tls.key: <base64-encoded-client-private-key>
//	  server.crt: <base64-encoded-server-certificate>
//	  server.key: <base64-encoded-server-private-key>
package secret

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/util"
)

type Controller struct {
	client.Client
	clock              clock.Clock
	workingNamespace   string
	serverSecretName   string
	serviceName        string
	expirationDuration time.Duration
}

func NewController(clk clock.Clock, kubeClient client.Client, ns, secretName, serviceName string, expirationDuration time.Duration) *Controller {
	return &Controller{
		clock:              clk,
		Client:             kubeClient,
		workingNamespace:   ns,
		serverSecretName:   secretName,
		serviceName:        serviceName,
		expirationDuration: expirationDuration,
	}
}

// Reconcile manages the lifecycle of TLS certificates.
// It ensures that certificates are renewed before they expire.
//
// The reconciliation logic:
// 1. Ignores secrets that don't match the expected name/namespace
// 2. Checks if existing certificates need renewal (>85% of validity period elapsed)
// 3. If renewal is not needed, schedules next check
// 4. If renewal is needed, generates new certificates and updates the secret
func (c *Controller) Reconcile(ctx context.Context, secret *corev1.Secret) (reconcile.Result, error) {
	// Only process the specific secret we're managing
	if secret.Namespace != c.workingNamespace || secret.Name != c.serverSecretName {
		return reconcile.Result{}, nil
	}

	ctx = util.WithControllerName(ctx, "server.secret")

	// Check if we have valid certificate data and whether it needs renewal
	if hasCertificateData(secret.Data) {
		// if the certificate has been valid for more than 85% of its total validity period, we will renew it.
		if shouldRenew, timeUntilNextCheck := shouldRenewCert(c.clock, secret.Data[util.ServerCert], secret.Data[util.ServerKey]); !shouldRenew {
			// Certificate is still valid, just schedule next check
			return reconcile.Result{RequeueAfter: timeUntilNextCheck}, nil
		}
	}

	// Certificate needs renewal or doesn't exist - generate new one
	updatedSecret := secret.DeepCopy()
	newSecret, err := generateSecret(ctx, c.serviceName, c.serverSecretName, c.workingNamespace, c.clock.Now().Add(c.expirationDuration))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to generate new secret: %w", err)
	}
	updatedSecret.Data = newSecret.Data

	// Update the secret with new certificate data
	if err := c.Client.Update(ctx, updatedSecret); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update secret %s/%s: %w", c.workingNamespace, c.serverSecretName, err)
	}

	return reconcile.Result{}, nil
}

// CertificateBundle contains all the certificates and keys needed for GRPC communication
type CertificateBundle struct {
	CACert     []byte
	ServerCert []byte
	ServerKey  []byte
	ClientCert []byte
	ClientKey  []byte
}

// generateSecret creates a new Kubernetes secret containing comprehensive TLS certificates
// for secure GRPC communication between KEDA core and the external Kaito scaler.
// It generates a complete certificate chain including:
// - CA Certificate: Root certificate authority for scaler communication
// - Server Certificate: Used by the external Kaito scaler GRPC server
// - Client Certificate: Used by KEDA core to authenticate with external scaler
// - DNS SANs: Includes service FQDN for flexible deployment
func generateSecret(ctx context.Context, serviceName, name, namespace string, notAfter time.Time) (*corev1.Secret, error) {
	certs, err := generateComprehensiveCerts(serviceName, namespace, notAfter)
	if err != nil {
		return nil, fmt.Errorf("failed to create comprehensive certificates for service %s in namespace %s: %w", serviceName, namespace, err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: "kubernetes.io/tls",
		Data: map[string][]byte{
			// Standard kubernetes.io/tls fields (client certificates)
			util.ClientCert: certs.ClientCert,
			util.ClientKey:  certs.ClientKey,

			// Server certificates for GRPC server
			util.ServerCert: certs.ServerCert,
			util.ServerKey:  certs.ServerKey,

			// CA certificate for trust chain
			util.CACert: certs.CACert,
		},
	}, nil
}

// generateComprehensiveCerts creates a complete certificate chain for secure GRPC communication
func generateComprehensiveCerts(serviceName, namespace string, notAfter time.Time) (*CertificateBundle, error) {
	// Generate CA private key
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	// Create CA certificate template
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"KEDA Kaito Scaler"},
			CommonName:   "KEDA Kaito Scaler CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              notAfter,
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	// Create CA certificate
	caBytes, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Encode CA certificate to PEM
	caCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	// Generate server private key
	serverPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate server private key: %w", err)
	}

	// Create server certificate template with comprehensive DNS SANs
	serverTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"KEDA Kaito Scaler"},
			CommonName:   serviceName,
		},
		NotBefore: time.Now(),
		NotAfter:  notAfter,
		DNSNames: []string{
			serviceName,
			fmt.Sprintf("%s.%s", serviceName, namespace),
			fmt.Sprintf("%s.%s.svc", serviceName, namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
			"localhost",
		},
		IPAddresses: []net.IP{
			net.IPv4(127, 0, 0, 1),
			net.IPv6loopback,
		},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	// Parse CA certificate for signing
	caCert, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Create server certificate
	serverBytes, err := x509.CreateCertificate(rand.Reader, &serverTemplate, caCert, &serverPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	// Encode server certificate and key to PEM
	serverCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverBytes,
	})

	serverKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverPrivKey),
	})

	// Generate client private key
	clientPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate client private key: %w", err)
	}

	// Create client certificate template
	clientTemplate := x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"KEDA Kaito Scaler"},
			CommonName:   "KEDA Core Client",
		},
		NotBefore:   time.Now(),
		NotAfter:    notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	// Create client certificate
	clientBytes, err := x509.CreateCertificate(rand.Reader, &clientTemplate, caCert, &clientPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create client certificate: %w", err)
	}

	// Encode client certificate and key to PEM
	clientCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: clientBytes,
	})

	clientKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientPrivKey),
	})

	return &CertificateBundle{
		CACert:     caCertPEM,
		ServerCert: serverCertPEM,
		ServerKey:  serverKeyPEM,
		ClientCert: clientCertPEM,
		ClientKey:  clientKeyPEM,
	}, nil
}

// hasCertificateData checks if the secret contains all required certificate data
func hasCertificateData(data map[string][]byte) bool {
	// Check for all required certificate fields
	requiredFields := []string{
		util.CACert,     // ca.crt
		util.ServerCert, // server.crt
		util.ServerKey,  // server.key
		util.ClientCert, // tls.crt
		util.ClientKey,  // tls.key
	}

	for _, field := range requiredFields {
		if len(data[field]) == 0 {
			return false
		}
	}

	return true
}

func shouldRenewCert(clk clock.Clock, certPEMBlock, keyPEMBlock []byte) (bool, time.Duration) {
	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return true, 0
	}

	certData, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return true, 0
	}

	// Additional certificate validation
	if err := validateCertificate(certData, clk.Now()); err != nil {
		return true, 0
	}

	now := clk.Now()
	startTime := certData.NotBefore
	expiryTime := certData.NotAfter
	totalValidity := expiryTime.Sub(startTime)
	elapsed := now.Sub(startTime)

	// Renew when certificate has been valid for 85% of its total validity period
	if float64(elapsed)/float64(totalValidity) >= 0.85 {
		return true, 0
	}

	// Calculate next check time (every 5% of total validity, minimum 1 minute)
	nextCheck := totalValidity / 20
	timeUntilNextCheck := expiryTime.Sub(now) - nextCheck
	if timeUntilNextCheck < time.Minute {
		timeUntilNextCheck = time.Minute
	}

	return false, timeUntilNextCheck
}

// validateCertificate performs additional certificate validation
func validateCertificate(cert *x509.Certificate, now time.Time) error {
	// Check if certificate is currently valid (not expired, not before valid time)
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate is not yet valid (NotBefore: %v, Now: %v)", cert.NotBefore, now)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate has expired (NotAfter: %v, Now: %v)", cert.NotAfter, now)
	}

	// Check key usage - should include digital signature and key encipherment for TLS
	hasDigitalSignature := cert.KeyUsage&x509.KeyUsageDigitalSignature != 0
	hasKeyEncipherment := cert.KeyUsage&x509.KeyUsageKeyEncipherment != 0

	if !hasDigitalSignature || !hasKeyEncipherment {
		return fmt.Errorf("certificate has invalid key usage (DigitalSignature: %v, KeyEncipherment: %v)",
			hasDigitalSignature, hasKeyEncipherment)
	}

	// Check extended key usage - should include server authentication
	hasServerAuth := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
			break
		}
	}
	if !hasServerAuth {
		return fmt.Errorf("certificate missing server authentication extended key usage")
	}

	return nil
}

func (c *Controller) generateSecretPredicateFunc() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			secret, ok := e.Object.(*corev1.Secret)
			if !ok {
				return false
			}

			return secret.Namespace == c.workingNamespace && secret.Name == c.serverSecretName
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			secret, ok := e.ObjectNew.(*corev1.Secret)
			if !ok {
				return false
			}

			return secret.Namespace == c.workingNamespace && secret.Name == c.serverSecretName
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=list;watch;get;update
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=mutatingwebhookconfigurations,verbs=list;watch;get;update
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=validatingwebhookconfigurations,verbs=list;watch;get;update

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("server.secret").
		For(&corev1.Secret{}, builder.WithPredicates(c.generateSecretPredicateFunc())).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, 300*time.Second),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 3,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
