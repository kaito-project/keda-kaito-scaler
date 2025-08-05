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
)

const (
	ServerKey  = "server-key.pem"
	ServerCert = "server-cert.pem"
	CACert     = "ca-cert.pem"
)

type controllerNameKeyType struct{}
type webhookNameKeyType struct{}

var (
	controllerNameKey = controllerNameKeyType{}
	webhookNameKey    = webhookNameKeyType{}
)

func WithControllerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, controllerNameKey, name)
}

func WithWebhookName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, webhookNameKey, name)
}
