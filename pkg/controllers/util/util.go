// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

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
