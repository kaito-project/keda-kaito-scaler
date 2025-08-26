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
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kaito-project/keda-kaito-scaler/pkg/controllers/autoprovision"
	"github.com/kaito-project/keda-kaito-scaler/pkg/util/controller"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=list;watch;get;update
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=mutatingwebhookconfigurations,verbs=list;watch;get;update
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=validatingwebhookconfigurations,verbs=list;watch;get;update

func NewControllers(mgr manager.Manager) []controller.Controller {
	return []controller.Controller{
		autoprovision.NewAutoProvisionController(mgr.GetClient()),
		// add controllers here
	}
}
