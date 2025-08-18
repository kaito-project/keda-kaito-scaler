package secret

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/util"
)

func setupTestController(t *testing.T) *Controller {
	t.Helper()
	clk := testingclock.NewFakeClock(time.Now())
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	kubeClient := fake.NewClientBuilder().WithScheme(s).Build()
	return NewController(clk, kubeClient, "test-namespace", "test-secret", "test-service", 24*time.Hour)
}

func TestNewController(t *testing.T) {
	clk := clock.RealClock{}
	kubeClient := fake.NewClientBuilder().Build()
	namespace := "test-namespace"
	secretName := "test-secret"
	serviceName := "test-service"
	expirationDuration := 24 * time.Hour

	controller := NewController(clk, kubeClient, namespace, secretName, serviceName, expirationDuration)

	if controller.clock != clk {
		t.Errorf("Expected clock to be %v, got %v", clk, controller.clock)
	}
	if controller.Client != kubeClient {
		t.Errorf("Expected client to be %v, got %v", kubeClient, controller.Client)
	}
	if controller.workingNamespace != namespace {
		t.Errorf("Expected namespace to be %s, got %s", namespace, controller.workingNamespace)
	}
	if controller.serverSecretName != secretName {
		t.Errorf("Expected secret name to be %s, got %s", secretName, controller.serverSecretName)
	}
	if controller.expirationDuration != expirationDuration {
		t.Errorf("Expected expiration duration to be %v, got %v", expirationDuration, controller.expirationDuration)
	}
}

func TestController_Reconcile_WrongSecretIgnored(t *testing.T) {
	controller := setupTestController(t)

	wrongSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-secret",
			Namespace: "test-namespace",
		},
	}

	result, err := controller.Reconcile(context.Background(), wrongSecret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should not requeue since we ignore this secret
	if result.RequeueAfter != 0 {
		t.Error("Expected no requeue for wrong secret")
	}
}

func TestController_Reconcile_EmptySecret(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{},
	}

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	result, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should not requeue since certificate was generated
	if result.RequeueAfter != 0 {
		t.Error("Expected no requeue after successful certificate generation")
	}

	// Verify secret was updated with certificate data
	var updatedSecret corev1.Secret
	if err := controller.Client.Get(ctx, client.ObjectKey{Name: "test-secret", Namespace: "test-namespace"}, &updatedSecret); err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	requiredFields := []string{util.CACert, util.ServerCert, util.ServerKey, util.ClientCert, util.ClientKey}
	for _, field := range requiredFields {
		if len(updatedSecret.Data[field]) == 0 {
			t.Errorf("Expected field %s to be populated", field)
		}
	}
}

func TestController_GenerateSecretPredicateFunc(t *testing.T) {
	controller := setupTestController(t)
	predicateFunc := controller.generateSecretPredicateFunc()

	t.Run("CreateFunc", func(t *testing.T) {
		validEvent := event.CreateEvent{
			Object: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-namespace",
				},
			},
		}

		if !predicateFunc.Create(validEvent) {
			t.Error("Expected CreateFunc to return true for valid secret")
		}

		invalidEvent := event.CreateEvent{
			Object: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-secret",
					Namespace: "test-namespace",
				},
			},
		}

		if predicateFunc.Create(invalidEvent) {
			t.Error("Expected CreateFunc to return false for invalid secret")
		}
	})

	t.Run("UpdateFunc", func(t *testing.T) {
		validEvent := event.UpdateEvent{
			ObjectNew: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-namespace",
				},
			},
		}

		if !predicateFunc.Update(validEvent) {
			t.Error("Expected UpdateFunc to return true for valid secret")
		}
	})

	t.Run("DeleteFunc", func(t *testing.T) {
		deleteEvent := event.DeleteEvent{}
		if predicateFunc.Delete(deleteEvent) {
			t.Error("Expected DeleteFunc to return false")
		}
	})
}

func TestGenerateSecret(t *testing.T) {
	ctx := context.Background()
	serviceName := "test-service"
	secretName := "test-secret"
	namespace := "test-namespace"
	notAfter := time.Now().Add(24 * time.Hour)

	secret, err := generateSecret(ctx, serviceName, secretName, namespace, notAfter)
	if err != nil {
		t.Fatalf("generateSecret() returned error: %v", err)
	}

	// Check secret metadata
	if secret.Name != secretName {
		t.Errorf("Expected name %s, got %s", secretName, secret.Name)
	}
	if secret.Namespace != namespace {
		t.Errorf("Expected namespace %s, got %s", namespace, secret.Namespace)
	}
	if secret.Type != "kubernetes.io/tls" {
		t.Errorf("Expected type kubernetes.io/tls, got %s", secret.Type)
	}

	// Check that all required fields are present
	requiredFields := []string{util.CACert, util.ServerCert, util.ServerKey, util.ClientCert, util.ClientKey}
	for _, field := range requiredFields {
		if len(secret.Data[field]) == 0 {
			t.Errorf("Expected field %s to be populated", field)
		}
	}
}

// Helper function to create a valid certificate for testing
func createTestCertificate(t *testing.T, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()

	// Generate private key
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	// Create certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
			CommonName:   "test.example.com",
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		DNSNames:    []string{"test.example.com"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	// Create certificate
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})

	return certPEM, keyPEM
}

func TestHasCertificateData(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string][]byte
		expected bool
	}{
		{
			name: "complete certificate data",
			data: map[string][]byte{
				util.CACert:     []byte("ca-cert"),
				util.ServerCert: []byte("server-cert"),
				util.ServerKey:  []byte("server-key"),
				util.ClientCert: []byte("client-cert"),
				util.ClientKey:  []byte("client-key"),
			},
			expected: true,
		},
		{
			name: "missing CA cert",
			data: map[string][]byte{
				util.ServerCert: []byte("server-cert"),
				util.ServerKey:  []byte("server-key"),
				util.ClientCert: []byte("client-cert"),
				util.ClientKey:  []byte("client-key"),
			},
			expected: false,
		},
		{
			name: "missing server cert",
			data: map[string][]byte{
				util.CACert:     []byte("ca-cert"),
				util.ServerKey:  []byte("server-key"),
				util.ClientCert: []byte("client-cert"),
				util.ClientKey:  []byte("client-key"),
			},
			expected: false,
		},
		{
			name:     "empty data",
			data:     map[string][]byte{},
			expected: false,
		},
		{
			name:     "nil data",
			data:     nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasCertificateData(tt.data)
			if result != tt.expected {
				t.Errorf("hasCertificateData() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestShouldRenewCert(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())

	t.Run("invalid certificate data", func(t *testing.T) {
		shouldRenew, duration := shouldRenewCert(clk, []byte("invalid"), []byte("invalid"))
		if !shouldRenew {
			t.Error("Expected shouldRenew to be true for invalid certificate data")
		}
		if duration != 0 {
			t.Errorf("Expected duration to be 0, got %v", duration)
		}
	})

	t.Run("valid certificate within renewal threshold", func(t *testing.T) {
		// Create a certificate that's valid for 24 hours, current time is 1 hour after start
		notBefore := clk.Now().Add(-1 * time.Hour)
		notAfter := clk.Now().Add(23 * time.Hour)
		certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

		shouldRenew, duration := shouldRenewCert(clk, certPEM, keyPEM)
		if shouldRenew {
			t.Error("Expected shouldRenew to be false for fresh certificate")
		}
		if duration <= 0 {
			t.Errorf("Expected positive duration until next check, got %v", duration)
		}
	})

	t.Run("certificate needs renewal (85% threshold)", func(t *testing.T) {
		// Create a certificate that's been valid for 21 hours out of 24 (87.5% > 85%)
		notBefore := clk.Now().Add(-21 * time.Hour)
		notAfter := clk.Now().Add(3 * time.Hour)
		certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

		shouldRenew, duration := shouldRenewCert(clk, certPEM, keyPEM)
		if !shouldRenew {
			t.Error("Expected shouldRenew to be true for certificate past 85% threshold")
		}
		if duration != 0 {
			t.Errorf("Expected duration to be 0 when renewal needed, got %v", duration)
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		// Create an expired certificate
		notBefore := clk.Now().Add(-48 * time.Hour)
		notAfter := clk.Now().Add(-1 * time.Hour)
		certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

		shouldRenew, duration := shouldRenewCert(clk, certPEM, keyPEM)
		if !shouldRenew {
			t.Error("Expected shouldRenew to be true for expired certificate")
		}
		if duration != 0 {
			t.Errorf("Expected duration to be 0 for expired certificate, got %v", duration)
		}
	})

	t.Run("certificate not yet valid", func(t *testing.T) {
		// Create a certificate that's not yet valid
		notBefore := clk.Now().Add(1 * time.Hour)
		notAfter := clk.Now().Add(25 * time.Hour)
		certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

		shouldRenew, duration := shouldRenewCert(clk, certPEM, keyPEM)
		if !shouldRenew {
			t.Error("Expected shouldRenew to be true for not-yet-valid certificate")
		}
		if duration != 0 {
			t.Errorf("Expected duration to be 0 for not-yet-valid certificate, got %v", duration)
		}
	})
}

func TestValidateCertificate(t *testing.T) {
	now := time.Now()

	t.Run("valid certificate", func(t *testing.T) {
		certPEM, _ := createTestCertificate(t, now.Add(-1*time.Hour), now.Add(23*time.Hour))

		// Parse the certificate
		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("Failed to parse test certificate: %v", err)
		}

		err = validateCertificate(cert, now)
		if err != nil {
			t.Errorf("Expected no error for valid certificate, got %v", err)
		}
	})

	t.Run("not yet valid certificate", func(t *testing.T) {
		certPEM, _ := createTestCertificate(t, now.Add(1*time.Hour), now.Add(25*time.Hour))

		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("Failed to parse test certificate: %v", err)
		}

		err = validateCertificate(cert, now)
		if err == nil {
			t.Error("Expected error for not-yet-valid certificate")
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		certPEM, _ := createTestCertificate(t, now.Add(-25*time.Hour), now.Add(-1*time.Hour))

		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("Failed to parse test certificate: %v", err)
		}

		err = validateCertificate(cert, now)
		if err == nil {
			t.Error("Expected error for expired certificate")
		}
	})

	t.Run("certificate with invalid key usage", func(t *testing.T) {
		// Create certificate with only digital signature, missing key encipherment
		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("Failed to generate private key: %v", err)
		}

		template := x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject: pkix.Name{
				Organization: []string{"Test Org"},
				CommonName:   "test.example.com",
			},
			NotBefore:   now.Add(-1 * time.Hour),
			NotAfter:    now.Add(23 * time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			KeyUsage:    x509.KeyUsageDigitalSignature, // Missing KeyEncipherment
			DNSNames:    []string{"test.example.com"},
		}

		certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
		if err != nil {
			t.Fatalf("Failed to create certificate: %v", err)
		}

		cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			t.Fatalf("Failed to parse certificate: %v", err)
		}

		err = validateCertificate(cert, now)
		if err == nil {
			t.Error("Expected error for certificate with invalid key usage")
		}
	})

	t.Run("certificate missing server auth extended key usage", func(t *testing.T) {
		// Create certificate with client auth but not server auth
		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("Failed to generate private key: %v", err)
		}

		template := x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject: pkix.Name{
				Organization: []string{"Test Org"},
				CommonName:   "test.example.com",
			},
			NotBefore:   now.Add(-1 * time.Hour),
			NotAfter:    now.Add(23 * time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, // Missing ServerAuth
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			DNSNames:    []string{"test.example.com"},
		}

		certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
		if err != nil {
			t.Fatalf("Failed to create certificate: %v", err)
		}

		cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			t.Fatalf("Failed to parse certificate: %v", err)
		}

		err = validateCertificate(cert, now)
		if err == nil {
			t.Error("Expected error for certificate missing server auth extended key usage")
		}
	})
}

func TestController_Reconcile_CertificateRenewal(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())
	controller := setupTestControllerWithClock(t, clk)
	ctx := context.Background()

	// Create a secret with valid but soon-to-expire certificates
	notBefore := clk.Now().Add(-21 * time.Hour) // 21 hours ago
	notAfter := clk.Now().Add(3 * time.Hour)    // 3 hours from now (87.5% elapsed > 85% threshold)
	certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{
			util.CACert:     []byte("ca-cert"),
			util.ServerCert: certPEM,
			util.ServerKey:  keyPEM,
			util.ClientCert: []byte("client-cert"),
			util.ClientKey:  []byte("client-key"),
		},
	}

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	result, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should not requeue since certificate was renewed
	if result.RequeueAfter != 0 {
		t.Error("Expected no requeue after certificate renewal")
	}

	// Verify secret was updated with new certificate data
	var updatedSecret corev1.Secret
	if err := controller.Client.Get(ctx, client.ObjectKey{Name: "test-secret", Namespace: "test-namespace"}, &updatedSecret); err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	// Check that the certificate was actually updated (different from original)
	if string(updatedSecret.Data[util.ServerCert]) == string(certPEM) {
		t.Error("Expected certificate to be renewed")
	}
}

func TestController_Reconcile_ValidCertificateSchedulesRecheck(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())
	controller := setupTestControllerWithClock(t, clk)
	ctx := context.Background()

	// Create a secret with fresh certificates (only 4% of validity period elapsed)
	notBefore := clk.Now().Add(-1 * time.Hour) // 1 hour ago
	notAfter := clk.Now().Add(23 * time.Hour)  // 23 hours from now (4% elapsed < 85% threshold)
	certPEM, keyPEM := createTestCertificate(t, notBefore, notAfter)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{
			util.CACert:     []byte("ca-cert"),
			util.ServerCert: certPEM,
			util.ServerKey:  keyPEM,
			util.ClientCert: []byte("client-cert"),
			util.ClientKey:  []byte("client-key"),
		},
	}

	result, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should schedule a requeue for next check
	if result.RequeueAfter <= 0 {
		t.Error("Expected positive requeue time for valid certificate")
	}
}

func TestController_Reconcile_UpdateError(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Create a secret but don't add it to the fake client to simulate update error
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{},
	}

	result, err := controller.Reconcile(ctx, secret)
	if err == nil {
		t.Error("Expected error when updating non-existent secret")
	}

	// Should not requeue on error
	if result.RequeueAfter != 0 {
		t.Error("Expected no requeue on error")
	}
}

func TestGenerateComprehensiveCerts(t *testing.T) {
	serviceName := "test-service"
	namespace := "test-namespace"
	notAfter := time.Now().Add(24 * time.Hour)

	certs, err := generateComprehensiveCerts(serviceName, namespace, notAfter)
	if err != nil {
		t.Fatalf("generateComprehensiveCerts() returned error: %v", err)
	}

	// Check that all certificates are present
	if len(certs.CACert) == 0 {
		t.Error("Expected CA certificate to be populated")
	}
	if len(certs.ServerCert) == 0 {
		t.Error("Expected server certificate to be populated")
	}
	if len(certs.ServerKey) == 0 {
		t.Error("Expected server key to be populated")
	}
	if len(certs.ClientCert) == 0 {
		t.Error("Expected client certificate to be populated")
	}
	if len(certs.ClientKey) == 0 {
		t.Error("Expected client key to be populated")
	}

	// Verify certificates can be parsed
	_, err = tls.X509KeyPair(certs.ServerCert, certs.ServerKey)
	if err != nil {
		t.Errorf("Failed to parse server certificate/key pair: %v", err)
	}

	_, err = tls.X509KeyPair(certs.ClientCert, certs.ClientKey)
	if err != nil {
		t.Errorf("Failed to parse client certificate/key pair: %v", err)
	}

	// Parse and verify server certificate has expected DNS names
	block, _ := pem.Decode(certs.ServerCert)
	serverCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse server certificate: %v", err)
	}

	expectedDNSNames := []string{
		serviceName,
		serviceName + "." + namespace,
		serviceName + "." + namespace + ".svc",
		serviceName + "." + namespace + ".svc.cluster.local",
		"localhost",
	}

	for _, expectedDNS := range expectedDNSNames {
		found := false
		for _, actualDNS := range serverCert.DNSNames {
			if actualDNS == expectedDNS {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected DNS name %s not found in certificate", expectedDNS)
		}
	}
}

func TestController_GenerateSecretPredicateFunc_EdgeCases(t *testing.T) {
	controller := setupTestController(t)
	predicateFunc := controller.generateSecretPredicateFunc()

	t.Run("CreateFunc with non-secret object", func(t *testing.T) {
		invalidEvent := event.CreateEvent{
			Object: &corev1.ConfigMap{}, // Not a secret
		}

		if predicateFunc.Create(invalidEvent) {
			t.Error("Expected CreateFunc to return false for non-secret object")
		}
	})

	t.Run("CreateFunc with wrong namespace", func(t *testing.T) {
		invalidEvent := event.CreateEvent{
			Object: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "wrong-namespace",
				},
			},
		}

		if predicateFunc.Create(invalidEvent) {
			t.Error("Expected CreateFunc to return false for wrong namespace")
		}
	})

	t.Run("UpdateFunc with non-secret object", func(t *testing.T) {
		invalidEvent := event.UpdateEvent{
			ObjectNew: &corev1.ConfigMap{}, // Not a secret
		}

		if predicateFunc.Update(invalidEvent) {
			t.Error("Expected UpdateFunc to return false for non-secret object")
		}
	})

	t.Run("UpdateFunc with wrong namespace", func(t *testing.T) {
		invalidEvent := event.UpdateEvent{
			ObjectNew: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "wrong-namespace",
				},
			},
		}

		if predicateFunc.Update(invalidEvent) {
			t.Error("Expected UpdateFunc to return false for wrong namespace")
		}
	})

	t.Run("UpdateFunc with wrong name", func(t *testing.T) {
		invalidEvent := event.UpdateEvent{
			ObjectNew: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-secret",
					Namespace: "test-namespace",
				},
			},
		}

		if predicateFunc.Update(invalidEvent) {
			t.Error("Expected UpdateFunc to return false for wrong secret name")
		}
	})
}

// Helper function to set up controller with specific clock
func setupTestControllerWithClock(t *testing.T, clk clock.Clock) *Controller {
	t.Helper()
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	kubeClient := fake.NewClientBuilder().WithScheme(s).Build()
	return NewController(clk, kubeClient, "test-namespace", "test-secret", "test-service", 24*time.Hour)
}

// Minimal fake manager implementation for testing
type fakeManager struct {
	scheme *runtime.Scheme
}

func (f *fakeManager) GetScheme() *runtime.Scheme {
	return f.scheme
}

func (f *fakeManager) GetClient() client.Client {
	return fake.NewClientBuilder().WithScheme(f.scheme).Build()
}

func (f *fakeManager) GetFieldIndexer() client.FieldIndexer {
	return nil
}

func (f *fakeManager) GetCache() interface{} {
	return nil
}

func (f *fakeManager) GetEventRecorderFor(name string) interface{} {
	return nil
}

func (f *fakeManager) GetRESTMapper() interface{} {
	return nil
}

func (f *fakeManager) GetAPIReader() client.Reader {
	return nil
}

func (f *fakeManager) Start(ctx context.Context) error {
	return nil
}

func (f *fakeManager) Add(runnable interface{}) error {
	// For testing, just return success
	return nil
}

func (f *fakeManager) Elected() <-chan struct{} {
	return nil
}

func (f *fakeManager) AddHealthzCheck(string, interface{}) error {
	return nil
}

func (f *fakeManager) AddReadyzCheck(string, interface{}) error {
	return nil
}

func (f *fakeManager) GetControllerOptions() interface{} {
	return nil
}

func (f *fakeManager) GetLogger() interface{} {
	return nil
}

func (f *fakeManager) GetWebhookServer() interface{} {
	return nil
}

func (f *fakeManager) GetHTTPClient() interface{} {
	return nil
}
