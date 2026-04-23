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

// Package cert provides helpers that load TLS material from Kubernetes
// Secrets with ResourceVersion-based caching, so the TLS GetCertificate
// callback does not reparse PEM on every handshake.
package cert

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// NewServerCertLoader returns a TLS GetCertificate callback that memoises the
// parsed certificate keyed on the source Secret's ResourceVersion. This avoids
// reparsing PEM bytes on every TLS handshake while still picking up rotations
// automatically (a new ResourceVersion invalidates the cache).
//
// certDataKey / keyDataKey name the fields inside secret.Data that hold the
// PEM-encoded certificate and key.
//
// Contract: returning (nil, nil) when the secret is not found yet surfaces to
// the TLS client as "tls: no certificates configured".
func NewServerCertLoader(lister corev1listers.SecretLister, namespace, secretName, certDataKey, keyDataKey string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	var (
		mu         sync.Mutex
		cachedRV   string
		cachedCert *tls.Certificate
	)

	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		secret, err := lister.Secrets(namespace).Get(secretName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil //nolint:nilerr
			}
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
		}

		rv := secret.ResourceVersion
		mu.Lock()
		defer mu.Unlock()
		if rv != "" && rv == cachedRV && cachedCert != nil {
			return cachedCert, nil
		}

		certPEM, ok := secret.Data[certDataKey]
		if !ok {
			return nil, fmt.Errorf("certificate field %q not found in secret %s/%s", certDataKey, namespace, secretName)
		}
		keyPEM, ok := secret.Data[keyDataKey]
		if !ok {
			return nil, fmt.Errorf("key field %q not found in secret %s/%s", keyDataKey, namespace, secretName)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to create X509 key pair: %w", err)
		}

		cachedCert = &cert
		cachedRV = rv
		return cachedCert, nil
	}
}

// NewRootCAsLoader returns a callback that assembles the trusted CA pool from
// the given secret, memoised on ResourceVersion. caDataKey names the field
// inside secret.Data that holds the PEM-encoded CA bundle.
func NewRootCAsLoader(lister corev1listers.SecretLister, namespace, secretName, caDataKey string) func() (*x509.CertPool, error) {
	var (
		mu         sync.Mutex
		cachedRV   string
		cachedPool *x509.CertPool
	)

	return func() (*x509.CertPool, error) {
		secret, err := lister.Secrets(namespace).Get(secretName)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
		}

		rv := secret.ResourceVersion
		mu.Lock()
		defer mu.Unlock()
		if rv != "" && rv == cachedRV && cachedPool != nil {
			return cachedPool, nil
		}

		caCertData, ok := secret.Data[caDataKey]
		if !ok {
			return nil, fmt.Errorf("CA certificate field %q not found in secret %s/%s", caDataKey, namespace, secretName)
		}
		if len(caCertData) == 0 {
			return nil, fmt.Errorf("CA certificate is empty in secret %s/%s", namespace, secretName)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCertData) {
			return nil, fmt.Errorf("failed to parse CA certificate from secret %s/%s", namespace, secretName)
		}

		cachedPool = pool
		cachedRV = rv
		return cachedPool, nil
	}
}
