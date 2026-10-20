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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

const (
	mcpGatewayExtCRD = "mcpgatewayextensions.mcp.kuadrant.io"
	gatewayCRD       = "gateways.gateway.networking.k8s.io"
)

var _ = Describe("Gateway Discovery", func() {
	ctx := context.Background()
	var gatewayTestNamespace string

	BeforeEach(func() {
		if !crdInstalled(ctx, mcpGatewayExtCRD) || !crdInstalled(ctx, gatewayCRD) {
			Skip("MCPGatewayExtension or Gateway CRD not installed")
		}

		prepareTest()
		gatewayTestNamespace = createTestNamespace(ctx, "mcplmo-e2e-gw-")
	})

	It("should discover MCPGatewayExtension and Gateway resources in status", func() {
		gw := newGatewayResource(gatewayTestNamespace, "test-gw", "mcp", "mcps")
		Expect(k8sClient.Create(ctx, gw)).To(Succeed())

		mcpge := newMCPGatewayExtensionResource(gatewayTestNamespace, "test-ext", "test-gw", gatewayTestNamespace, "mcp")
		Expect(k8sClient.Create(ctx, mcpge)).To(Succeed())

		By("Verifying status.availableMCPGateways contains the discovered gateway")
		Eventually(func(g Gomega) {
			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, cr)).To(Succeed())

			g.Expect(cr.Status.AvailableMCPGateways).To(ContainElement(SatisfyAll(
				HaveField("Name", "test-ext"),
				HaveField("Namespace", gatewayTestNamespace),
				HaveField("Gateway.Name", "test-gw"),
				HaveField("Gateway.Namespace", gatewayTestNamespace),
				HaveField("Gateway.Listeners", ContainElements(
					SatisfyAll(HaveField("Name", "mcp"), HaveField("Hostname", "*.mcp.example.com")),
					SatisfyAll(HaveField("Name", "mcps"), HaveField("Hostname", "*.mcps.example.com")),
				)),
			)))
		}, timeout, interval).Should(Succeed())
	})

	It("should update status when MCPGatewayExtension is removed", func() {
		gw := newGatewayResource(gatewayTestNamespace, "test-gw", "mcp")
		Expect(k8sClient.Create(ctx, gw)).To(Succeed())

		mcpge := newMCPGatewayExtensionResource(gatewayTestNamespace, "test-ext", "test-gw", gatewayTestNamespace, "mcp")
		Expect(k8sClient.Create(ctx, mcpge)).To(Succeed())

		By("Waiting for the gateway to appear in status")
		Eventually(func(g Gomega) {
			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, cr)).To(Succeed())
			g.Expect(cr.Status.AvailableMCPGateways).To(ContainElement(SatisfyAll(
				HaveField("Name", "test-ext"),
				HaveField("Namespace", gatewayTestNamespace),
			)))
		}, timeout, interval).Should(Succeed())

		By("Deleting the MCPGatewayExtension")
		Expect(k8sClient.Delete(ctx, mcpge)).To(Succeed())

		By("Verifying status.availableMCPGateways becomes empty")
		Eventually(func(g Gomega) {
			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, cr)).To(Succeed())
			g.Expect(cr.Status.AvailableMCPGateways).NotTo(ContainElement(SatisfyAll(
				HaveField("Name", "test-ext"),
				HaveField("Namespace", gatewayTestNamespace),
			)))
		}, timeout, interval).Should(Succeed())
	})
})

func crdInstalled(ctx context.Context, name string) bool {
	crd := &extv1.CustomResourceDefinition{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, crd)
	return err == nil
}

func newGatewayResource(namespace, name string, listeners ...string) *unstructured.Unstructured {
	listenerList := make([]interface{}, len(listeners))
	for i, l := range listeners {
		listenerList[i] = map[string]interface{}{
			"name":     l,
			"hostname": "*." + l + ".example.com",
			"port":     int64(80),
			"protocol": "HTTP",
		}
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"spec": map[string]interface{}{
			"gatewayClassName": "istio",
			"listeners":        listenerList,
		},
	}}
}

func newMCPGatewayExtensionResource(namespace, name, gwName, gwNamespace, sectionName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "mcp.kuadrant.io/v1",
		"kind":       "MCPGatewayExtension",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{
				"group":       "gateway.networking.k8s.io",
				"kind":        "Gateway",
				"name":        gwName,
				"namespace":   gwNamespace,
				"sectionName": sectionName,
			},
		},
	}}
}
