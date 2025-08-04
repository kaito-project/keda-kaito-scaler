// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package webhooks

import (
	"github.com/awslabs/operatorpkg/controller"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func NewWebhooks(mgr manager.Manager, clk clock.Clock) []controller.Controller {

	return []controller.Controller{
		// add webhooks here
	}
}
