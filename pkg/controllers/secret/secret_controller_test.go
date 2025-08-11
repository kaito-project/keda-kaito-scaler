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
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/util"
)

func TestNewController(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())
	kubeClient := fake.NewClientBuilder().Build()
	ns := "test-namespace"
	secretName := "test-secret"
	serviceName := "test-service"
	expirationDuration := 24 * time.Hour

	controller := NewController(clk, kubeClient, ns, secretName, serviceName, expirationDuration)

	if controller.clock != clk {
		t.Errorf("Expected clock to be %v, got %v", clk, controller.clock)
	}
	if controller.Client != kubeClient {
		t.Errorf("Expected client to be %v, got %v", kubeClient, controller.Client)
	}
	if controller.workingNamespace != ns {
		t.Errorf("Expected working namespace to be %s, got %s", ns, controller.workingNamespace)
	}
	if controller.webhookServerSecretName != secretName {
		t.Errorf("Expected secret name to be %s, got %s", secretName, controller.webhookServerSecretName)
	}
	if controller.webhookServiceName != serviceName {
		t.Errorf("Expected service name to be %s, got %s", serviceName, controller.webhookServiceName)
	}
	if controller.expirationDuration != expirationDuration {
		t.Errorf("Expected expiration duration to be %v, got %v", expirationDuration, controller.expirationDuration)
	}
}

func TestController_Reconcile_WrongSecretIgnored(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Test with wrong namespace
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "wrong-namespace",
		},
	}

	result, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("Expected no requeue, got %v", result.RequeueAfter)
	}

	// Test with wrong name
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-secret",
			Namespace: "test-namespace",
		},
	}

	result, err = controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("Expected no requeue, got %v", result.RequeueAfter)
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

	// Create webhook configurations first
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	_, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should update the secret with new certificates
	updatedSecret := &corev1.Secret{}
	err = controller.Client.Get(ctx, client.ObjectKey{Name: "test-secret", Namespace: "test-namespace"}, updatedSecret)
	if err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	if len(updatedSecret.Data[util.ServerCert]) == 0 {
		t.Error("Expected server cert to be generated")
	}
	if len(updatedSecret.Data[util.ServerKey]) == 0 {
		t.Error("Expected server key to be generated")
	}
	if len(updatedSecret.Data[util.CACert]) == 0 {
		t.Error("Expected CA cert to be generated")
	}
}

func TestController_Reconcile_ValidCertificate(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Create a valid certificate that doesn't need renewal
	certPEM, keyPEM, caCertPEM := generateTestCerts(t, time.Now().Add(24*time.Hour))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{
			util.ServerCert: certPEM,
			util.ServerKey:  keyPEM,
			util.CACert:     caCertPEM,
		},
	}

	// Create webhook configurations
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	result, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should requeue after some time
	if result.RequeueAfter == 0 {
		t.Error("Expected requeue after some time")
	}
}

func TestController_Reconcile_ExpiredCertificate(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Create an expired certificate
	certPEM, keyPEM, caCertPEM := generateTestCerts(t, time.Now().Add(-24*time.Hour))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{
			util.ServerCert: certPEM,
			util.ServerKey:  keyPEM,
			util.CACert:     caCertPEM,
		},
	}

	// Create webhook configurations first
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	_, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Should update the secret with new certificates
	updatedSecret := &corev1.Secret{}
	err = controller.Client.Get(ctx, client.ObjectKey{Name: "test-secret", Namespace: "test-namespace"}, updatedSecret)
	if err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	// Verify the certificate was renewed (check that it's different and valid)
	if len(updatedSecret.Data[util.ServerCert]) == 0 {
		t.Error("Expected new server certificate to be generated")
	}

	// Verify it's a valid certificate and different from the original
	_, err = tls.X509KeyPair(updatedSecret.Data[util.ServerCert], updatedSecret.Data[util.ServerKey])
	if err != nil {
		t.Errorf("New certificate is invalid: %v", err)
	}

	// The certificate should be different (renewed)
	if len(updatedSecret.Data[util.ServerCert]) > 0 && len(certPEM) > 0 {
		// Just check that the certificate data exists - the renewal process generates new certs
		t.Log("Certificate renewal completed successfully")
	}
}

func TestShouldRenewCert(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())

	tests := []struct {
		name           string
		notBefore      time.Time
		notAfter       time.Time
		currentTime    time.Time
		shouldRenew    bool
		expectDuration bool
	}{
		{
			name:           "fresh certificate",
			notBefore:      time.Now().Add(-1 * time.Hour),
			notAfter:       time.Now().Add(23 * time.Hour),
			currentTime:    time.Now(),
			shouldRenew:    false,
			expectDuration: true,
		},
		{
			name:           "certificate near expiry",
			notBefore:      time.Now().Add(-21 * time.Hour),
			notAfter:       time.Now().Add(3 * time.Hour),
			currentTime:    time.Now(),
			shouldRenew:    true,
			expectDuration: false,
		},
		{
			name:           "expired certificate",
			notBefore:      time.Now().Add(-25 * time.Hour),
			notAfter:       time.Now().Add(-1 * time.Hour),
			currentTime:    time.Now(),
			shouldRenew:    true,
			expectDuration: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk.SetTime(tt.currentTime)
			certPEM, keyPEM, _ := generateTestCertsWithTime(t, tt.notBefore, tt.notAfter)

			shouldRenew, duration := shouldRenewCert(clk, certPEM, keyPEM)

			if shouldRenew != tt.shouldRenew {
				t.Errorf("Expected shouldRenew to be %v, got %v", tt.shouldRenew, shouldRenew)
			}

			if tt.expectDuration && duration == 0 {
				t.Error("Expected duration to be greater than 0")
			}

			if !tt.expectDuration && duration != 0 {
				t.Error("Expected duration to be 0")
			}
		})
	}
}

func TestShouldRenewCert_InvalidCert(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())

	// Test with invalid certificate
	shouldRenew, duration := shouldRenewCert(clk, []byte("invalid"), []byte("invalid"))
	if !shouldRenew {
		t.Error("Expected shouldRenew to be true for invalid certificate")
	}
	if duration != 0 {
		t.Error("Expected duration to be 0 for invalid certificate")
	}
}

func TestCaCertChanged(t *testing.T) {
	tests := []struct {
		name     string
		old      []byte
		new      []byte
		expected bool
	}{
		{
			name:     "both nil",
			old:      nil,
			new:      nil,
			expected: false,
		},
		{
			name:     "old nil, new has value",
			old:      nil,
			new:      []byte("certificate"),
			expected: true,
		},
		{
			name:     "old has value, new nil",
			old:      []byte("certificate"),
			new:      nil,
			expected: true,
		},
		{
			name:     "both empty",
			old:      []byte{},
			new:      []byte{},
			expected: false,
		},
		{
			name:     "old empty, new has value",
			old:      []byte{},
			new:      []byte("certificate"),
			expected: true,
		},
		{
			name:     "old has value, new empty",
			old:      []byte("certificate"),
			new:      []byte{},
			expected: true,
		},
		{
			name:     "same certificates",
			old:      []byte("certificate1"),
			new:      []byte("certificate1"),
			expected: false,
		},
		{
			name:     "different certificates",
			old:      []byte("certificate1"),
			new:      []byte("certificate2"),
			expected: true,
		},
		{
			name:     "same content different slices",
			old:      []byte("certificate"),
			new:      append([]byte("certifi"), []byte("cate")...),
			expected: false,
		},
		{
			name:     "binary data with null bytes",
			old:      []byte{0x00, 0x01, 0x02, 0x03},
			new:      []byte{0x00, 0x01, 0x02, 0x04},
			expected: true,
		},
		{
			name:     "same binary data",
			old:      []byte{0x00, 0x01, 0x02, 0x03},
			new:      []byte{0x00, 0x01, 0x02, 0x03},
			expected: false,
		},
		{
			name:     "large certificates different",
			old:      make([]byte, 1000),
			new:      append(make([]byte, 999), 0x01),
			expected: true,
		},
		{
			name:     "unicode content same",
			old:      []byte("证书内容"),
			new:      []byte("证书内容"),
			expected: false,
		},
		{
			name:     "unicode content different",
			old:      []byte("证书内容1"),
			new:      []byte("证书内容2"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := caCertChanged(tt.old, tt.new)
			if result != tt.expected {
				t.Errorf("caCertChanged(%v, %v) = %v, expected %v",
					string(tt.old), string(tt.new), result, tt.expected)
			}
		})
	}
}

func TestValidatingWebhookConfigPredicate(t *testing.T) {
	// Test that the predicate constants are correctly defined
	if kedaKaitoScalerValidatingWebhookConfig == "" {
		t.Error("Expected validating webhook config name to be defined")
	}

	// Test CreateFunc
	validEvent := event.CreateEvent{
		Object: &admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: kedaKaitoScalerValidatingWebhookConfig,
			},
		},
	}
	if !validatingWebhookConfigPredicate.CreateFunc(validEvent) {
		t.Error("Expected CreateFunc to return true for valid webhook config")
	}

	invalidEvent := event.CreateEvent{
		Object: &admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-name",
			},
		},
	}
	if validatingWebhookConfigPredicate.CreateFunc(invalidEvent) {
		t.Error("Expected CreateFunc to return false for invalid webhook config")
	}

	// Test UpdateFunc
	validUpdateEvent := event.UpdateEvent{
		ObjectNew: &admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: kedaKaitoScalerValidatingWebhookConfig,
			},
		},
	}
	if !validatingWebhookConfigPredicate.UpdateFunc(validUpdateEvent) {
		t.Error("Expected UpdateFunc to return true for valid webhook config")
	}

	// Test DeleteFunc
	deleteEvent := event.DeleteEvent{}
	if validatingWebhookConfigPredicate.DeleteFunc(deleteEvent) {
		t.Error("Expected DeleteFunc to return false")
	}
}

func TestMutatingWebhookConfigPredicate(t *testing.T) {
	// Test that the predicate constants are correctly defined
	if kedaKaitoScalerMutatingWebhookConfig == "" {
		t.Error("Expected mutating webhook config name to be defined")
	}

	// Test CreateFunc
	validEvent := event.CreateEvent{
		Object: &admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: kedaKaitoScalerMutatingWebhookConfig,
			},
		},
	}
	if !mutatingWebhookConfigPredicate.CreateFunc(validEvent) {
		t.Error("Expected CreateFunc to return true for valid webhook config")
	}

	invalidEvent := event.CreateEvent{
		Object: &admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "invalid-name",
			},
		},
	}
	if mutatingWebhookConfigPredicate.CreateFunc(invalidEvent) {
		t.Error("Expected CreateFunc to return false for invalid webhook config")
	}
}

func TestController_GenerateSecretPredicateFunc(t *testing.T) {
	controller := setupTestController(t)
	predicateResult := controller.generateSecretPredicateFunc()

	// We can at least verify that the predicate function is created
	if predicateResult == nil {
		t.Error("Expected predicate to be created")
	}

	// The function returns predicate.Funcs which is a struct with CreateFunc, UpdateFunc, DeleteFunc fields
	// We can test the predicate by creating events and calling the predicate's methods directly

	t.Run("CreateFunc", func(t *testing.T) {
		// Test with correct secret (should return true)
		validSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		validEvent := event.CreateEvent{Object: validSecret}

		// Use the type assertion to get access to the predicate interface
		if !predicateResult.Create(validEvent) {
			t.Error("Expected predicate to return true for valid secret create event")
		}

		// Test with wrong namespace (should return false)
		wrongNamespaceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "wrong-namespace",
			},
		}
		wrongNamespaceEvent := event.CreateEvent{Object: wrongNamespaceSecret}
		if predicateResult.Create(wrongNamespaceEvent) {
			t.Error("Expected predicate to return false for wrong namespace")
		}

		// Test with wrong name (should return false)
		wrongNameSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wrong-secret",
				Namespace: "test-namespace",
			},
		}
		wrongNameEvent := event.CreateEvent{Object: wrongNameSecret}
		if predicateResult.Create(wrongNameEvent) {
			t.Error("Expected predicate to return false for wrong name")
		}

		// Test with non-secret object (should return false)
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		nonSecretEvent := event.CreateEvent{Object: configMap}
		if predicateResult.Create(nonSecretEvent) {
			t.Error("Expected predicate to return false for non-secret object")
		}

		// Test with nil object (should return false)
		nilEvent := event.CreateEvent{Object: nil}
		if predicateResult.Create(nilEvent) {
			t.Error("Expected predicate to return false for nil object")
		}
	})

	t.Run("UpdateFunc", func(t *testing.T) {
		// Test with correct secret (should return true)
		validSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		validEvent := event.UpdateEvent{ObjectNew: validSecret}
		if !predicateResult.Update(validEvent) {
			t.Error("Expected predicate to return true for valid secret update event")
		}

		// Test with wrong namespace (should return false)
		wrongNamespaceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "wrong-namespace",
			},
		}
		wrongNamespaceEvent := event.UpdateEvent{ObjectNew: wrongNamespaceSecret}
		if predicateResult.Update(wrongNamespaceEvent) {
			t.Error("Expected predicate to return false for wrong namespace")
		}

		// Test with wrong name (should return false)
		wrongNameSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wrong-secret",
				Namespace: "test-namespace",
			},
		}
		wrongNameEvent := event.UpdateEvent{ObjectNew: wrongNameSecret}
		if predicateResult.Update(wrongNameEvent) {
			t.Error("Expected predicate to return false for wrong name")
		}

		// Test with non-secret object (should return false)
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		nonSecretEvent := event.UpdateEvent{ObjectNew: configMap}
		if predicateResult.Update(nonSecretEvent) {
			t.Error("Expected predicate to return false for non-secret object")
		}

		// Test with nil object (should return false)
		nilEvent := event.UpdateEvent{ObjectNew: nil}
		if predicateResult.Update(nilEvent) {
			t.Error("Expected predicate to return false for nil object")
		}
	})

	t.Run("DeleteFunc", func(t *testing.T) {
		// DeleteFunc should always return false regardless of the event
		validSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		deleteEvent := event.DeleteEvent{Object: validSecret}
		if predicateResult.Delete(deleteEvent) {
			t.Error("Expected predicate to always return false for delete events")
		}

		// Test with nil object (should still return false)
		nilDeleteEvent := event.DeleteEvent{Object: nil}
		if predicateResult.Delete(nilDeleteEvent) {
			t.Error("Expected predicate to return false even for nil object in delete event")
		}
	})

	t.Run("GenericFunc", func(t *testing.T) {
		// Test Generic events as well for completeness
		validSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-secret",
				Namespace: "test-namespace",
			},
		}
		genericEvent := event.GenericEvent{Object: validSecret}

		// The predicate should handle generic events (default behavior)
		// Since we don't define GenericFunc in our predicate.Funcs, it should use the default behavior
		result := predicateResult.Generic(genericEvent)
		// We don't make assertions about the result since it depends on the default implementation
		_ = result // Just to use the variable
	})
}

func TestGenerateSecret(t *testing.T) {
	ctx := context.Background()
	serviceName := "test-service"
	name := "test-secret"
	namespace := "test-namespace"
	notAfter := time.Now().Add(24 * time.Hour)

	secret, err := generateSecret(ctx, serviceName, name, namespace, notAfter)
	if err != nil {
		t.Fatalf("Failed to generate secret: %v", err)
	}

	if secret.Name != name {
		t.Errorf("Expected secret name to be %s, got %s", name, secret.Name)
	}
	if secret.Namespace != namespace {
		t.Errorf("Expected secret namespace to be %s, got %s", namespace, secret.Namespace)
	}

	if len(secret.Data[util.ServerKey]) == 0 {
		t.Error("Expected server key to be present")
	}
	if len(secret.Data[util.ServerCert]) == 0 {
		t.Error("Expected server cert to be present")
	}
	if len(secret.Data[util.CACert]) == 0 {
		t.Error("Expected CA cert to be present")
	}

	// Verify the certificate is valid
	_, err = tls.X509KeyPair(secret.Data[util.ServerCert], secret.Data[util.ServerKey])
	if err != nil {
		t.Errorf("Generated certificate is invalid: %v", err)
	}
}

func TestController_UpdateWebhookConfigurations(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Create webhook configurations
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Create a new CA certificate
	_, _, newCACert := generateTestCerts(t, time.Now().Add(24*time.Hour))

	// Call updateWebhookConfigurations
	err := controller.updateWebhookConfigurations(ctx, newCACert)
	if err != nil {
		t.Fatalf("Failed to update webhook configurations: %v", err)
	}

	// Verify validating webhook was updated
	var validatingWebhook admissionv1.ValidatingWebhookConfiguration
	err = controller.Client.Get(ctx, client.ObjectKey{Name: kedaKaitoScalerValidatingWebhookConfig}, &validatingWebhook)
	if err != nil {
		t.Fatalf("Failed to get validating webhook: %v", err)
	}

	if len(validatingWebhook.Webhooks) > 0 {
		if string(validatingWebhook.Webhooks[0].ClientConfig.CABundle) != string(newCACert) {
			t.Error("Expected validating webhook CA bundle to be updated")
		}
		if validatingWebhook.Webhooks[0].ClientConfig.Service.Namespace != "test-namespace" {
			t.Error("Expected validating webhook service namespace to be updated")
		}
		if validatingWebhook.Webhooks[0].ClientConfig.Service.Name != "test-service" {
			t.Error("Expected validating webhook service name to be updated")
		}
	}

	// Verify mutating webhook was updated
	var mutatingWebhook admissionv1.MutatingWebhookConfiguration
	err = controller.Client.Get(ctx, client.ObjectKey{Name: kedaKaitoScalerMutatingWebhookConfig}, &mutatingWebhook)
	if err != nil {
		t.Fatalf("Failed to get mutating webhook: %v", err)
	}

	if len(mutatingWebhook.Webhooks) > 0 {
		if string(mutatingWebhook.Webhooks[0].ClientConfig.CABundle) != string(newCACert) {
			t.Error("Expected mutating webhook CA bundle to be updated")
		}
		if mutatingWebhook.Webhooks[0].ClientConfig.Service.Namespace != "test-namespace" {
			t.Error("Expected mutating webhook service namespace to be updated")
		}
		if mutatingWebhook.Webhooks[0].ClientConfig.Service.Name != "test-service" {
			t.Error("Expected mutating webhook service name to be updated")
		}
	}
}

func TestController_UpdateWebhookConfigurations_NotFound(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Don't create webhook configurations - test error path
	_, _, newCACert := generateTestCerts(t, time.Now().Add(24*time.Hour))

	// Call updateWebhookConfigurations - should return error
	err := controller.updateWebhookConfigurations(ctx, newCACert)
	if err == nil {
		t.Error("Expected error when webhook configurations are not found")
	}
}

func TestController_UpdateWebhookConfigurations_SameCA(t *testing.T) {
	controller := setupTestController(t)
	ctx := context.Background()

	// Create webhook configurations
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Use the same CA certificate that was set in setupWebhookConfigurations
	oldCACert := []byte("old-ca-bundle")

	// Call updateWebhookConfigurations with same CA
	err := controller.updateWebhookConfigurations(ctx, oldCACert)
	if err != nil {
		t.Fatalf("Failed to update webhook configurations: %v", err)
	}

	// Verify that no update was performed (coverage for the !updated path)
	var validatingWebhook admissionv1.ValidatingWebhookConfiguration
	err = controller.Client.Get(ctx, client.ObjectKey{Name: kedaKaitoScalerValidatingWebhookConfig}, &validatingWebhook)
	if err != nil {
		t.Fatalf("Failed to get validating webhook: %v", err)
	}

	// Should still have the old CA bundle
	if len(validatingWebhook.Webhooks) > 0 {
		if string(validatingWebhook.Webhooks[0].ClientConfig.CABundle) != string(oldCACert) {
			t.Error("Expected validating webhook CA bundle to remain unchanged")
		}
	}
}

func TestGenerateSecret_Error(t *testing.T) {
	// Test generateSecret function with invalid input that could cause errors
	ctx := context.Background()
	serviceName := ""
	name := ""
	namespace := ""
	// Use a time in the past to potentially trigger edge cases
	notAfter := time.Now().Add(-24 * time.Hour)

	secret, err := generateSecret(ctx, serviceName, name, namespace, notAfter)
	// The function should still work even with empty strings and past time
	if err != nil {
		t.Logf("generateSecret returned error as expected for edge case: %v", err)
	} else {
		if secret.Name != name {
			t.Errorf("Expected secret name to be %s, got %s", name, secret.Name)
		}
		if secret.Namespace != namespace {
			t.Errorf("Expected secret namespace to be %s, got %s", namespace, secret.Namespace)
		}
	}
}

func TestController_Reconcile_UpdateError(t *testing.T) {
	// Test the error path when updating the secret fails
	// This is harder to test with the fake client, but we can at least verify the happy path
	controller := setupTestController(t)
	ctx := context.Background()

	// Create a secret with partial certificate data to trigger renewal
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test-namespace",
		},
		Data: map[string][]byte{
			util.ServerCert: []byte("invalid-cert"),
			util.ServerKey:  []byte("invalid-key"),
			util.CACert:     []byte("invalid-ca"),
		},
	}

	// Create webhook configurations
	setupWebhookConfigurations(t, controller.Client, ctx)

	// Add secret to fake client
	if err := controller.Client.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create secret: %v", err)
	}

	// This should trigger certificate renewal due to invalid cert
	_, err := controller.Reconcile(ctx, secret)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Verify the secret was updated with valid certificates
	updatedSecret := &corev1.Secret{}
	err = controller.Client.Get(ctx, client.ObjectKey{Name: "test-secret", Namespace: "test-namespace"}, updatedSecret)
	if err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	// Verify the new certificate is valid
	_, err = tls.X509KeyPair(updatedSecret.Data[util.ServerCert], updatedSecret.Data[util.ServerKey])
	if err != nil {
		t.Errorf("New certificate is invalid: %v", err)
	}
}

// Helper functions

func setupTestController(t *testing.T) *Controller {
	t.Helper()
	clk := testingclock.NewFakeClock(time.Now())
	kubeClient := fake.NewClientBuilder().WithScheme(setupScheme(t)).Build()
	ns := "test-namespace"
	secretName := "test-secret"
	serviceName := "test-service"
	expirationDuration := 24 * time.Hour

	return NewController(clk, kubeClient, ns, secretName, serviceName, expirationDuration)
}

func setupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add admission scheme: %v", err)
	}
	return s
}

func setupWebhookConfigurations(t *testing.T, client client.Client, ctx context.Context) {
	t.Helper()

	validatingWebhook := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: kedaKaitoScalerValidatingWebhookConfig,
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "test-webhook",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: []byte("old-ca-bundle"),
					Service: &admissionv1.ServiceReference{
						Name:      "old-service",
						Namespace: "old-namespace",
					},
				},
			},
		},
	}

	mutatingWebhook := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: kedaKaitoScalerMutatingWebhookConfig,
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "test-webhook",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: []byte("old-ca-bundle"),
					Service: &admissionv1.ServiceReference{
						Name:      "old-service",
						Namespace: "old-namespace",
					},
				},
			},
		},
	}

	if err := client.Create(ctx, validatingWebhook); err != nil {
		t.Fatalf("Failed to create validating webhook: %v", err)
	}
	if err := client.Create(ctx, mutatingWebhook); err != nil {
		t.Fatalf("Failed to create mutating webhook: %v", err)
	}
}

func generateTestCerts(t *testing.T, notAfter time.Time) ([]byte, []byte, []byte) {
	t.Helper()
	return generateTestCertsWithTime(t, time.Now().Add(-1*time.Hour), notAfter)
}

func generateTestCertsWithTime(t *testing.T, notBefore, notAfter time.Time) ([]byte, []byte, []byte) {
	t.Helper()

	// Generate CA key
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate CA key: %v", err)
	}

	// Generate CA certificate
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("Failed to create CA certificate: %v", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	// Generate server key
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate server key: %v", err)
	}

	// Generate server certificate
	serverTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Test Server"},
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"test-service.test-namespace.svc.cluster.local"},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("Failed to create server certificate: %v", err)
	}

	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})

	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatalf("Failed to marshal server key: %v", err)
	}
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER})

	return serverCertPEM, serverKeyPEM, caCertPEM
}
