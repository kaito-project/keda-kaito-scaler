// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package controllers

import (
	"github.com/awslabs/operatorpkg/controller"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kaito-project/keda-kaito-scaler/cmd/app/options"
	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/secret"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch;update

func NewControllers(mgr manager.Manager, clock clock.Clock, opts *options.KedaKaitoScalerOptions) []controller.Controller {

	return []controller.Controller{
		secret.NewController(clock, mgr.GetClient(), opts.WorkingNamespace, opts.WebhookSecretName, opts.WebhookServiceName, opts.ExpirationDuration),
		// add controllers here
	}
}
