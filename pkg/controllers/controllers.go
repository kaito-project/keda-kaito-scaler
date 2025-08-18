// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
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
		secret.NewController(clock, mgr.GetClient(), opts.WorkingNamespace, opts.ScalerSecretName, opts.ScalerServiceName, opts.ExpirationDuration),
		// add controllers here
	}
}
