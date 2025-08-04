// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package secret

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"time"

	"golang.org/x/time/rate"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/webhook/certificates/resources"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/util"
)

const (
	kedaKaitoScalerValidatingWebhookConfig = "keda-kaito-scaler-validating-webhook-configuration"
	kedaKaitoScalerMutatingWebhookConfig   = "keda-kaito-scaler-mutating-webhook-configuration"
)

var (
	validatingWebhookConfigPredicate = predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			vwc, ok := e.Object.(*admissionv1.ValidatingWebhookConfiguration)
			if !ok {
				return false
			}

			return vwc.Name == kedaKaitoScalerValidatingWebhookConfig
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			vwc, ok := e.ObjectNew.(*admissionv1.ValidatingWebhookConfiguration)
			if !ok {
				return false
			}

			return vwc.Name == kedaKaitoScalerValidatingWebhookConfig
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}

	mutatingWebhookConfigPredicate = predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			mwc, ok := e.Object.(*admissionv1.MutatingWebhookConfiguration)
			if !ok {
				return false
			}

			return mwc.Name == kedaKaitoScalerMutatingWebhookConfig
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			mwc, ok := e.ObjectNew.(*admissionv1.MutatingWebhookConfiguration)
			if !ok {
				return false
			}

			return mwc.Name == kedaKaitoScalerMutatingWebhookConfig
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
)

type Controller struct {
	client.Client
	clock                   clock.Clock
	workingNamespace        string
	webhookServerSecretName string
	webhookServiceName      string
	expirationDuration      time.Duration
}

func NewController(clk clock.Clock, kubeClient client.Client, ns, secretName, serviceName string, expirationDuration time.Duration) *Controller {
	return &Controller{
		clock:                   clk,
		Client:                  kubeClient,
		workingNamespace:        ns,
		webhookServerSecretName: secretName,
		webhookServiceName:      serviceName,
		expirationDuration:      expirationDuration,
	}
}

// Reconcile manages the lifecycle of TLS certificates for webhook configurations.
// It ensures that certificates are renewed before they expire and that webhook
// configurations are updated with the latest CA certificates.
//
// The reconciliation logic:
// 1. Ignores secrets that don't match the expected name/namespace
// 2. Checks if existing certificates need renewal (>85% of validity period elapsed)
// 3. If renewal is not needed, updates webhook configurations and schedules next check
// 4. If renewal is needed, generates new certificates and updates both secret and webhook configs
func (c *Controller) Reconcile(ctx context.Context, secret *corev1.Secret) (reconcile.Result, error) {
	// Only process the specific secret we're managing
	if secret.Namespace != c.workingNamespace || secret.Name != c.webhookServerSecretName {
		return reconcile.Result{}, nil
	}

	ctx = util.WithControllerName(ctx, "server.secret")

	// Check if we have valid certificate data and whether it needs renewal
	if len(secret.Data[util.ServerCert]) != 0 &&
		len(secret.Data[util.ServerKey]) != 0 &&
		len(secret.Data[util.CACert]) != 0 {
		// if the certificate has been valid for more than 85% of its total validity period, we will renew it.
		if shouldRenew, timeUntilNextCheck := shouldRenewCert(c.clock, secret.Data[util.ServerCert], secret.Data[util.ServerKey]); !shouldRenew {
			// Certificate is still valid, just update webhook configurations and requeue for next check
			if err := c.updateWebhookConfigurations(ctx, secret.Data[util.CACert]); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to update webhook configurations: %w", err)
			}
			return reconcile.Result{RequeueAfter: timeUntilNextCheck}, nil
		}
	}

	// Certificate needs renewal or doesn't exist - generate new one
	updatedSecret := secret.DeepCopy()
	newSecret, err := generateSecret(ctx, c.webhookServiceName, c.webhookServerSecretName, c.workingNamespace, c.clock.Now().Add(c.expirationDuration))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to generate new secret: %w", err)
	}
	updatedSecret.Data = newSecret.Data

	// Update the secret with new certificate data
	if err := c.Client.Update(ctx, updatedSecret); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update secret %s/%s: %w", c.workingNamespace, c.webhookServerSecretName, err)
	}

	// Update webhook configurations with the new CA certificate
	return reconcile.Result{}, c.updateWebhookConfigurations(ctx, updatedSecret.Data[util.CACert])
}

// generateSecret creates a new Kubernetes secret containing TLS certificates for the webhook server.
// It generates a complete certificate chain including server certificate, private key, and CA certificate.
func generateSecret(ctx context.Context, serviceName, name, namespace string, notAfter time.Time) (*corev1.Secret, error) {
	serverKey, serverCert, caCert, err := resources.CreateCerts(ctx, serviceName, namespace, notAfter)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificates for service %s in namespace %s: %w", serviceName, namespace, err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			util.ServerKey:  serverKey,
			util.ServerCert: serverCert,
			util.CACert:     caCert,
		},
	}, nil
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

func (c *Controller) updateWebhookConfigurations(ctx context.Context, caCert []byte) error {
	// Update validating webhook configuration
	if err := c.updateValidatingWebhookConfiguration(ctx, caCert); err != nil {
		return fmt.Errorf("failed to update validating webhook configuration: %w", err)
	}

	// Update mutating webhook configuration
	if err := c.updateMutatingWebhookConfiguration(ctx, caCert); err != nil {
		return fmt.Errorf("failed to update mutating webhook configuration: %w", err)
	}

	return nil
}

func (c *Controller) updateValidatingWebhookConfiguration(ctx context.Context, caCert []byte) error {
	var webhook admissionv1.ValidatingWebhookConfiguration
	if err := c.Client.Get(ctx, client.ObjectKey{Name: kedaKaitoScalerValidatingWebhookConfig}, &webhook); err != nil {
		return err
	}

	updated := false
	for i := range webhook.Webhooks {
		if caCertChanged(webhook.Webhooks[i].ClientConfig.CABundle, caCert) {
			webhook.Webhooks[i].ClientConfig.CABundle = caCert
			webhook.Webhooks[i].ClientConfig.Service.Namespace = c.workingNamespace
			webhook.Webhooks[i].ClientConfig.Service.Name = c.webhookServiceName
			updated = true
		}
	}

	if updated {
		if err := c.Client.Update(ctx, &webhook); err != nil {
			log.FromContext(ctx).Error(err, "failed to update validating webhook configuration", "name", kedaKaitoScalerValidatingWebhookConfig)
			return err
		}
		log.FromContext(ctx).Info("updated validating webhook configuration", "name", kedaKaitoScalerValidatingWebhookConfig)
	}

	return nil
}

func (c *Controller) updateMutatingWebhookConfiguration(ctx context.Context, caCert []byte) error {
	var webhook admissionv1.MutatingWebhookConfiguration
	if err := c.Client.Get(ctx, client.ObjectKey{Name: kedaKaitoScalerMutatingWebhookConfig}, &webhook); err != nil {
		return err
	}

	updated := false
	for i := range webhook.Webhooks {
		if caCertChanged(webhook.Webhooks[i].ClientConfig.CABundle, caCert) {
			webhook.Webhooks[i].ClientConfig.CABundle = caCert
			webhook.Webhooks[i].ClientConfig.Service.Namespace = c.workingNamespace
			webhook.Webhooks[i].ClientConfig.Service.Name = c.webhookServiceName
			updated = true
		}
	}

	if updated {
		if err := c.Client.Update(ctx, &webhook); err != nil {
			log.FromContext(ctx).Error(err, "failed to update mutating webhook configuration", "name", kedaKaitoScalerMutatingWebhookConfig)
			return err
		}
		log.FromContext(ctx).Info("updated mutating webhook configuration", "name", kedaKaitoScalerMutatingWebhookConfig)
	}

	return nil
}

// caCertChanged compares two CA certificates to determine if they are different.
// It uses base64 encoding comparison for efficiency, but could be enhanced to
// compare actual certificate content for semantic equivalence.
func caCertChanged(old, new []byte) bool {
	// Handle nil cases
	if old == nil && new == nil {
		return false
	}
	if old == nil || new == nil {
		return true
	}

	// Compare base64 encoded versions
	oldEncoded := base64.StdEncoding.EncodeToString(old)
	newEncoded := base64.StdEncoding.EncodeToString(new)

	return oldEncoded != newEncoded
}

func (c *Controller) generateSecretPredicateFunc() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			secret, ok := e.Object.(*corev1.Secret)
			if !ok {
				return false
			}

			return secret.Namespace == c.workingNamespace && secret.Name == c.webhookServerSecretName
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			secret, ok := e.ObjectNew.(*corev1.Secret)
			if !ok {
				return false
			}

			return secret.Namespace == c.workingNamespace && secret.Name == c.webhookServerSecretName
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
		Watches(&admissionv1.ValidatingWebhookConfiguration{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{Namespace: c.workingNamespace, Name: c.webhookServerSecretName},
				},
			}
		}), builder.WithPredicates(validatingWebhookConfigPredicate)).
		Watches(&admissionv1.MutatingWebhookConfiguration{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{Namespace: c.workingNamespace, Name: c.webhookServerSecretName},
				},
			}
		}), builder.WithPredicates(mutatingWebhookConfigPredicate)).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, 300*time.Second),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
			MaxConcurrentReconciles: 3,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
