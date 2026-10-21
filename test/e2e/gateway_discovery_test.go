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
	"testing"

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

// prepareGatewayTest skips when the optional gateway CRDs are absent and
// otherwise returns a fixture namespace that is deleted after the test.
func prepareGatewayTest(t *testing.T) string {
	t.Helper()
	prepareSuite(t)
	ctx := context.Background()
	if !crdInstalled(ctx, mcpGatewayExtCRD) || !crdInstalled(ctx, gatewayCRD) {
		t.Skip("MCPGatewayExtension or Gateway CRD not installed")
	}

	waitForOperandReady(t, ctx)
	return createTestNamespace(t, ctx, "mcplmo-e2e-gw-")
}

// TestE2EGatewayDiscovery verifies MCPGatewayExtension and Gateway resources are discovered in status.
func TestE2EGatewayDiscovery(t *testing.T) {
	gatewayTestNamespace := prepareGatewayTest(t)
	g := NewWithT(t)
	ctx := context.Background()

	gw := newGatewayResource(gatewayTestNamespace, "test-gw", "mcp", "mcps")
	g.Expect(k8sClient.Create(ctx, gw)).To(Succeed())

	mcpge := newMCPGatewayExtensionResource(gatewayTestNamespace, "test-ext", "test-gw", gatewayTestNamespace, "mcp")
	g.Expect(k8sClient.Create(ctx, mcpge)).To(Succeed())

	t.Log("Verifying status.availableMCPGateways contains the discovered gateway")
	g.Eventually(func(g Gomega) {
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
}

// TestE2EGatewayExtensionRemoval verifies status is updated when an MCPGatewayExtension is removed.
func TestE2EGatewayExtensionRemoval(t *testing.T) {
	gatewayTestNamespace := prepareGatewayTest(t)
	g := NewWithT(t)
	ctx := context.Background()

	gw := newGatewayResource(gatewayTestNamespace, "test-gw", "mcp")
	g.Expect(k8sClient.Create(ctx, gw)).To(Succeed())

	mcpge := newMCPGatewayExtensionResource(gatewayTestNamespace, "test-ext", "test-gw", gatewayTestNamespace, "mcp")
	g.Expect(k8sClient.Create(ctx, mcpge)).To(Succeed())

	t.Log("Waiting for the gateway to appear in status")
	g.Eventually(func(g Gomega) {
		cr := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		g.Expect(cr.Status.AvailableMCPGateways).To(ContainElement(SatisfyAll(
			HaveField("Name", "test-ext"),
			HaveField("Namespace", gatewayTestNamespace),
		)))
	}, timeout, interval).Should(Succeed())

	t.Log("Deleting the MCPGatewayExtension")
	g.Expect(k8sClient.Delete(ctx, mcpge)).To(Succeed())

	t.Log("Verifying status.availableMCPGateways becomes empty")
	g.Eventually(func(g Gomega) {
		cr := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		g.Expect(cr.Status.AvailableMCPGateways).NotTo(ContainElement(SatisfyAll(
			HaveField("Name", "test-ext"),
			HaveField("Namespace", gatewayTestNamespace),
		)))
	}, timeout, interval).Should(Succeed())
}

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
