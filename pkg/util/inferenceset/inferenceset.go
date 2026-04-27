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

package inferenceset

import (
	"context"
	"fmt"

	kaitov1alpha1 "github.com/kaito-project/kaito/api/v1alpha1"
	kaitov1beta1 "github.com/kaito-project/kaito/api/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkspaceCreatedByInferenceSetLabel marks Workspaces created by an InferenceSet.
// Value is the InferenceSet name.
const WorkspaceCreatedByInferenceSetLabel = "inferenceset.kaito.sh/created-by"

// ListWorkspaces lists Workspaces in iObj.Namespace created by the given InferenceSet.
func ListWorkspaces(ctx context.Context, iObj *kaitov1alpha1.InferenceSet, kubeClient client.Client) (*kaitov1beta1.WorkspaceList, error) {
	if iObj == nil {
		return nil, fmt.Errorf("InferenceSet object is nil")
	}

	workspaceList := &kaitov1beta1.WorkspaceList{}
	selector := labels.SelectorFromSet(labels.Set{
		WorkspaceCreatedByInferenceSetLabel: iObj.Name,
	})

	err := retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return true
	}, func() error {
		return kubeClient.List(ctx, workspaceList,
			client.InNamespace(iObj.Namespace),
			&client.MatchingLabelsSelector{Selector: selector},
		)
	})
	if err != nil {
		return nil, err
	}
	return workspaceList, nil
}
