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

package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// --- fake lister -----------------------------------------------------------

type fakeSecretLister struct {
	secrets map[string]*corev1.Secret // key "ns/name"
}

func (f *fakeSecretLister) List(labels.Selector) (ret []*corev1.Secret, err error) {
	for _, s := range f.secrets {
		ret = append(ret, s)
	}
	return ret, nil
}

func (f *fakeSecretLister) Secrets(namespace string) corev1listers.SecretNamespaceLister {
	return &fakeSecretNamespaceLister{parent: f, namespace: namespace}
}

type fakeSecretNamespaceLister struct {
	parent    *fakeSecretLister
	namespace string
}

func (f *fakeSecretNamespaceLister) List(labels.Selector) ([]*corev1.Secret, error) {
	var out []*corev1.Secret
	for key, s := range f.parent.secrets {
		if key[:len(f.namespace)+1] == f.namespace+"/" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeSecretNamespaceLister) Get(name string) (*corev1.Secret, error) {
	if s, ok := f.parent.secrets[f.namespace+"/"+name]; ok {
		return s, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "secrets"}, name)
}

func newLister() *fakeSecretLister {
	return &fakeSecretLister{secrets: map[string]*corev1.Secret{}}
}

func (f *fakeSecretLister) set(ns, name, rv string, data map[string][]byte) {
	f.secrets[ns+"/"+name] = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, ResourceVersion: rv},
		Data:       data,
	}
}

// --- cert fixture ----------------------------------------------------------

// makeCertPair returns a self-signed server cert + matching private key, both
// PEM encoded. A fresh pair is generated per call so tests can't rely on
// cross-test state.
func makeCertPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "unit-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// --- NewServerCertLoader --------------------------------------------------

func TestNewServerCertLoader_NotFoundReturnsNil(t *testing.T) {
	lister := newLister()
	load := NewServerCertLoader(lister, "ns", "missing", "tls.crt", "tls.key")

	cert, err := load(nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cert != nil {
		t.Fatalf("want nil cert for missing secret, got %#v", cert)
	}
}

func TestNewServerCertLoader_MissingFields(t *testing.T) {
	lister := newLister()
	certPEM, keyPEM := makeCertPair(t)

	lister.set("ns", "s", "1", map[string][]byte{"other": certPEM})
	load := NewServerCertLoader(lister, "ns", "s", "tls.crt", "tls.key")
	if _, err := load(nil); err == nil {
		t.Fatal("expected error when cert field missing")
	}

	lister.set("ns", "s", "2", map[string][]byte{"tls.crt": certPEM})
	if _, err := load(nil); err == nil {
		t.Fatal("expected error when key field missing")
	}

	// Sanity: when both are present, the same loader succeeds.
	lister.set("ns", "s", "3", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM})
	if _, err := load(nil); err != nil {
		t.Fatalf("unexpected err with both fields: %v", err)
	}
}

func TestNewServerCertLoader_InvalidPEM(t *testing.T) {
	lister := newLister()
	lister.set("ns", "s", "1", map[string][]byte{
		"tls.crt": []byte("not a cert"),
		"tls.key": []byte("not a key"),
	})
	load := NewServerCertLoader(lister, "ns", "s", "tls.crt", "tls.key")
	if _, err := load(nil); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewServerCertLoader_CachesByResourceVersion(t *testing.T) {
	lister := newLister()
	certPEM, keyPEM := makeCertPair(t)
	lister.set("ns", "s", "1", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM})

	load := NewServerCertLoader(lister, "ns", "s", "tls.crt", "tls.key")

	first, err := load(nil)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := load(nil)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first != second {
		t.Fatalf("expected cached pointer reuse, got different *tls.Certificate")
	}

	// Corrupting secret.Data with same RV must still return the cached cert:
	// the cache short-circuits the PEM read entirely.
	lister.set("ns", "s", "1", map[string][]byte{"tls.crt": []byte("bad"), "tls.key": []byte("bad")})
	cached, err := load(nil)
	if err != nil {
		t.Fatalf("cached load should not reparse: %v", err)
	}
	if cached != first {
		t.Fatal("expected still-cached pointer on same RV")
	}
}

func TestNewServerCertLoader_InvalidatesOnResourceVersionChange(t *testing.T) {
	lister := newLister()
	certPEM, keyPEM := makeCertPair(t)
	lister.set("ns", "s", "1", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM})

	load := NewServerCertLoader(lister, "ns", "s", "tls.crt", "tls.key")
	first, err := load(nil)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Rotate: new key pair, new ResourceVersion.
	cert2, key2 := makeCertPair(t)
	lister.set("ns", "s", "2", map[string][]byte{"tls.crt": cert2, "tls.key": key2})

	rotated, err := load(nil)
	if err != nil {
		t.Fatalf("rotated load: %v", err)
	}
	if rotated == first {
		t.Fatal("expected new pointer after ResourceVersion change")
	}
}

// --- NewRootCAsLoader -----------------------------------------------------

func TestNewRootCAsLoader_SecretNotFoundIsError(t *testing.T) {
	lister := newLister()
	load := NewRootCAsLoader(lister, "ns", "missing", "ca.crt")
	if _, err := load(); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestNewRootCAsLoader_MissingOrEmptyCAField(t *testing.T) {
	lister := newLister()

	lister.set("ns", "s", "1", map[string][]byte{"other": []byte("x")})
	load := NewRootCAsLoader(lister, "ns", "s", "ca.crt")
	if _, err := load(); err == nil {
		t.Fatal("expected error when CA field missing")
	}

	lister.set("ns", "s", "2", map[string][]byte{"ca.crt": {}})
	if _, err := load(); err == nil {
		t.Fatal("expected error when CA field empty")
	}
}

func TestNewRootCAsLoader_InvalidPEM(t *testing.T) {
	lister := newLister()
	lister.set("ns", "s", "1", map[string][]byte{"ca.crt": []byte("garbage")})
	load := NewRootCAsLoader(lister, "ns", "s", "ca.crt")
	if _, err := load(); err == nil {
		t.Fatal("expected error for garbage CA bundle")
	}
}

func TestNewRootCAsLoader_CachesAndInvalidates(t *testing.T) {
	lister := newLister()
	caPEM, _ := makeCertPair(t) // self-signed cert reused as CA bundle
	lister.set("ns", "s", "1", map[string][]byte{"ca.crt": caPEM})

	load := NewRootCAsLoader(lister, "ns", "s", "ca.crt")
	first, err := load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first != second {
		t.Fatal("expected cached pool pointer reuse on same RV")
	}

	// Rotate the CA bundle: new RV must invalidate the cache.
	caPEM2, _ := makeCertPair(t)
	lister.set("ns", "s", "2", map[string][]byte{"ca.crt": caPEM2})
	rotated, err := load()
	if err != nil {
		t.Fatalf("rotated load: %v", err)
	}
	if rotated == first {
		t.Fatal("expected new pool pointer after RV change")
	}
}
