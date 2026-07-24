/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Module Operator Deployment", func() {
	ctx := context.Background()

	It("should have app.kubernetes.io/name label on the pod template", func() {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: operandNamespace,
			Name:      moduleOperatorDeployment,
		}, dep)).To(Succeed())

		Expect(dep.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-module-operator"))
		Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-module-operator"))
	})
})
