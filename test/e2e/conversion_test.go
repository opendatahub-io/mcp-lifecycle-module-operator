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
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("MCPServer conversion", func() {
	ctx := context.Background()

	BeforeEach(prepareTest)

	It("serves a v1alpha1-created MCPServer at both versions via the conversion webhook", func() {
		fixtureNamespace := createTestNamespace(ctx, "mcplmo-e2e-conversion-")

		const serverName = "conversion"
		const serverPort = int64(8080)

		By("Verifying the shipped MCPServer CRD declares a Webhook conversion strategy")
		Eventually(func(g Gomega) {
			crd := &extv1.CustomResourceDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
			g.Expect(crd.Spec.Conversion).NotTo(BeNil())
			g.Expect(crd.Spec.Conversion.Strategy).To(Equal(extv1.WebhookConverter))
		}, timeout, interval).Should(Succeed())

		By("Creating an MCPServer at mcp.x-k8s.io/v1alpha1 (up-converts on write)")
		alpha := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "mcp.x-k8s.io/v1alpha1",
			"kind":       "MCPServer",
			"metadata": map[string]interface{}{
				"name":      serverName,
				"namespace": fixtureNamespace,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"type":           "ContainerImage",
					"containerImage": map[string]interface{}{"ref": "quay.io/containers/kubernetes_mcp_server:latest"},
				},
				"config":  map[string]interface{}{"port": serverPort},
				"runtime": map[string]interface{}{"replicas": int64(0)},
			},
		}}
		// Retry tolerates webhook warmup; treat AlreadyExists as success so a
		// Create that persisted server-side but returned a transient client error
		// does not wedge the retry loop until timeout.
		Eventually(func(g Gomega) {
			err := k8sClient.Create(ctx, alpha.DeepCopy())
			if k8serr.IsAlreadyExists(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}, timeout, interval).Should(Succeed())

		By("Reading it back at mcp.x-k8s.io/v1beta1 (the stored version)")
		beta := &unstructured.Unstructured{}
		beta.SetGroupVersionKind(schema.GroupVersionKind{Group: "mcp.x-k8s.io", Version: "v1beta1", Kind: "MCPServer"})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: fixtureNamespace, Name: serverName}, beta)).To(Succeed())
			expectMCPServerSpec(g, beta, serverPort)
		}, timeout, interval).Should(Succeed())

		By("Reading it back at mcp.x-k8s.io/v1alpha1 (down-converts on read)")
		roundTrip := &unstructured.Unstructured{}
		roundTrip.SetGroupVersionKind(schema.GroupVersionKind{Group: "mcp.x-k8s.io", Version: "v1alpha1", Kind: "MCPServer"})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: fixtureNamespace, Name: serverName}, roundTrip)).To(Succeed())
			expectMCPServerSpec(g, roundTrip, serverPort)
		}, timeout, interval).Should(Succeed())
	})
})

func expectMCPServerSpec(g Gomega, obj *unstructured.Unstructured, wantPort int64) {
	port, found, err := unstructured.NestedInt64(obj.Object, "spec", "config", "port")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(port).To(Equal(wantPort))

	sourceType, found, err := unstructured.NestedString(obj.Object, "spec", "source", "type")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(sourceType).To(Equal("ContainerImage"))
}
