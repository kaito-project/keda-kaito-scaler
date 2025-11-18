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

package util

import (
	"context"
	"time"
)

const (
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
	ControllerFieldOwner = "keda-kaito-scaler"

	ServerCertDir = "/tmp/keda-kaito-scaler-certs/server"
	ClientCertDir = "/tmp/keda-kaito-scaler-certs/client"
)

type controllerNameKeyType struct{}

var (
	controllerNameKey = controllerNameKeyType{}
)

func WithControllerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, controllerNameKey, name)
}
