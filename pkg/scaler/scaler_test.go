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
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewKaitoScaler(t *testing.T) {
	// Create a fake Kubernetes client
	fakeClient := fake.NewClientBuilder().Build()

	// Call the function under test
	scaler := NewKaitoScaler(fakeClient)

	// Verify the returned scaler is not nil
	if scaler == nil {
		t.Fatal("NewKaitoScaler returned nil")
	}

	// Verify the kubeClient is set correctly
	if scaler.kubeClient != fakeClient {
		t.Error("kubeClient was not set correctly")
	}

	// Verify httpTransport configuration
	if scaler.httpTransport == nil {
		t.Fatal("httpTransport is nil")
	}
	if scaler.httpTransport.MaxIdleConns != 20 {
		t.Errorf("httpTransport.MaxIdleConns = %d, want 20", scaler.httpTransport.MaxIdleConns)
	}
	if scaler.httpTransport.MaxIdleConnsPerHost != 5 {
		t.Errorf("httpTransport.MaxIdleConnsPerHost = %d, want 5", scaler.httpTransport.MaxIdleConnsPerHost)
	}
	if scaler.httpTransport.IdleConnTimeout != 30*time.Second {
		t.Errorf("httpTransport.IdleConnTimeout = %v, want %v", scaler.httpTransport.IdleConnTimeout, 30*time.Second)
	}

	// Verify tlsTransport configuration
	if scaler.tlsTransport == nil {
		t.Fatal("tlsTransport is nil")
	}
	if scaler.tlsTransport.MaxIdleConns != 20 {
		t.Errorf("tlsTransport.MaxIdleConns = %d, want 20", scaler.tlsTransport.MaxIdleConns)
	}
	if scaler.tlsTransport.MaxIdleConnsPerHost != 5 {
		t.Errorf("tlsTransport.MaxIdleConnsPerHost = %d, want 5", scaler.tlsTransport.MaxIdleConnsPerHost)
	}
	if scaler.tlsTransport.IdleConnTimeout != 30*time.Second {
		t.Errorf("tlsTransport.IdleConnTimeout = %v, want %v", scaler.tlsTransport.IdleConnTimeout, 30*time.Second)
	}

	// Verify TLS configuration
	if scaler.tlsTransport.TLSClientConfig == nil {
		t.Fatal("tlsTransport.TLSClientConfig is nil")
	}
	if !scaler.tlsTransport.TLSClientConfig.InsecureSkipVerify {
		t.Error("tlsTransport.TLSClientConfig.InsecureSkipVerify should be true")
	}
}

func TestNewKaitoScaler_WithNilClient(t *testing.T) {
	// Test with nil client
	scaler := NewKaitoScaler(nil)

	// Verify the returned scaler is not nil even with nil client
	if scaler == nil {
		t.Fatal("NewKaitoScaler returned nil")
	}

	// Verify the kubeClient is nil as passed
	if scaler.kubeClient != nil {
		t.Error("kubeClient should be nil")
	}

	// Verify transports are still initialized
	if scaler.httpTransport == nil {
		t.Error("httpTransport should not be nil")
	}
	if scaler.tlsTransport == nil {
		t.Error("tlsTransport should not be nil")
	}
}
