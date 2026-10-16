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
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Restricted operand NetworkPolicy posture", func() {
	ctx := context.Background()

	const (
		mcpServerName = "mcplmo-e2e-netpol"
		// LabelKeyMCPServer is the operand's per-server workload label; the
		// generated Deployment and Pods carry mcp-server=<MCPServer name>.
		labelKeyMCPServer = "mcp-server"
		// metadataNameLabel is the immutable label every namespace carries, used
		// to select the operator namespace as an ingress peer without mutating
		// any shared cluster state.
		metadataNameLabel = "kubernetes.io/metadata.name"
	)

	BeforeEach(prepareTest)

	It("admits only the operator's own controller pod for an MCPServer without an ingress source", func() {
		fixtureNamespace := createTestNamespace(ctx, "mcplmo-e2e-netpol-")
		By("Creating an MCPServer with no spec.network.ingressFrom")
		server := newMCPServer(fixtureNamespace, mcpServerName, nil)
		Expect(k8sClient.Create(ctx, server)).To(Succeed())

		By("Verifying the operand admits only the operator controller pod (self-peer)")
		// Under the restricted posture an MCPServer that declares no ingress
		// source is not left fully deny-by-default: the operand emits a single
		// ingress rule admitting the operator's own controller pod, and denies
		// every other source. That self-peer AND's a namespaceSelector on the
		// operator namespace with a podSelector on the operator Deployment labels,
		// so the operator can still reach the operand to run its verification
		// handshake even though no explicit source was declared.
		netpol := &networkingv1.NetworkPolicy{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: fixtureNamespace,
				Name:      mcpServerName,
			}, netpol)).To(Succeed())

			g.Expect(netpol.Spec.PolicyTypes).To(ConsistOf(
				networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))

			g.Expect(netpol.Spec.Ingress).To(HaveLen(1),
				"expected a single operator self-peer ingress rule, got %#v", netpol.Spec.Ingress)
			from := netpol.Spec.Ingress[0].From
			g.Expect(from).To(HaveLen(1))
			g.Expect(from[0].NamespaceSelector).NotTo(BeNil())
			g.Expect(from[0].NamespaceSelector.MatchLabels).To(
				HaveKeyWithValue(metadataNameLabel, operandNamespace))
			g.Expect(from[0].PodSelector).NotTo(BeNil())
			g.Expect(from[0].PodSelector.MatchLabels).To(
				HaveKeyWithValue("control-plane", "controller-manager"))
			g.Expect(from[0].PodSelector.MatchLabels).To(
				HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-operator"))

			// Egress stays allow-all by default: a single empty rule.
			g.Expect(netpol.Spec.Egress).To(HaveLen(1))
			g.Expect(netpol.Spec.Egress[0].To).To(BeEmpty())
			g.Expect(netpol.Spec.Egress[0].Ports).To(BeEmpty())
		}, timeout, interval).Should(Succeed())

		By("Verifying the workload stays healthy and the server is verified via the self-peer")
		// kubelet readiness/liveness probes originate from the node, not the pod
		// network, so they are not subject to NetworkPolicy and the workload stays
		// Available. The server also reaches Verified without any explicit ingress
		// source, because the self-peer carries the operator's handshake. As in the
		// ingressFrom spec, Verified is only surfaced on the v1beta1 representation.
		Eventually(func(g Gomega) {
			deps := &appsv1.DeploymentList{}
			g.Expect(k8sClient.List(ctx, deps,
				client.InNamespace(fixtureNamespace),
				client.MatchingLabels{labelKeyMCPServer: mcpServerName},
			)).To(Succeed())
			g.Expect(deps.Items).To(HaveLen(1))
			g.Expect(deps.Items[0].Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
		}, timeout, interval).Should(Succeed())

		verified := &unstructured.Unstructured{}
		verified.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "mcp.x-k8s.io",
			Version: "v1beta1",
			Kind:    "MCPServer",
		})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: fixtureNamespace,
				Name:      mcpServerName,
			}, verified)).To(Succeed())

			conditions, found, err := unstructured.NestedSlice(verified.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue(), "MCPServer %q has no status.conditions yet", mcpServerName)

			g.Expect(conditionStatus(conditions, "Verified")).To(Equal("True"),
				"expected Verified=True via self-peer, conditions: %#v", conditions)
			g.Expect(conditionStatus(conditions, "Available")).To(Equal("True"),
				"expected Available=True, conditions: %#v", conditions)
		}, timeout, interval).Should(Succeed())
	})

	It("honors spec.network.ingressFrom under restricted posture", func() {
		fixtureNamespace := createTestNamespace(ctx, "mcplmo-e2e-netpol-")
		By("Creating an MCPServer that admits the operand namespace")
		ingressFrom := []interface{}{
			map[string]interface{}{
				"namespaceSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						metadataNameLabel: operandNamespace,
					},
				},
			},
		}
		server := newMCPServer(fixtureNamespace, mcpServerName, ingressFrom)
		Expect(k8sClient.Create(ctx, server)).To(Succeed())

		By("Verifying the NetworkPolicy carries the configured ingress peer")
		netpol := &networkingv1.NetworkPolicy{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: fixtureNamespace,
				Name:      mcpServerName,
			}, netpol)).To(Succeed())

			g.Expect(netpol.Spec.Ingress).To(HaveLen(1))
			from := netpol.Spec.Ingress[0].From
			g.Expect(from).To(HaveLen(1))
			g.Expect(from[0].NamespaceSelector).NotTo(BeNil())
			g.Expect(from[0].NamespaceSelector.MatchLabels).To(
				HaveKeyWithValue(metadataNameLabel, operandNamespace))
		}, timeout, interval).Should(Succeed())

		By("Verifying the server becomes Verified and Available once the ingress path is open")
		// Admitting the operator namespace as an ingress peer lets the operator
		// complete its MCP protocol handshake to the operand pod. This proves the
		// restricted posture does not break verification once a valid ingress
		// path is open: the server still becomes Verified (the operator reached
		// the endpoint over the open ingress path) and Available.
		//
		// The handshake-based Verified condition is only surfaced on the
		// mcp.x-k8s.io/v1beta1 representation; the deprecated v1alpha1 conversion
		// drops Verified and renames Available to Ready, so the status is read
		// via v1beta1 where both conditions are exposed.
		verified := &unstructured.Unstructured{}
		verified.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "mcp.x-k8s.io",
			Version: "v1beta1",
			Kind:    "MCPServer",
		})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: fixtureNamespace,
				Name:      mcpServerName,
			}, verified)).To(Succeed())

			conditions, found, err := unstructured.NestedSlice(verified.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue(), "MCPServer %q has no status.conditions yet", mcpServerName)

			g.Expect(conditionStatus(conditions, "Verified")).To(Equal("True"),
				"expected Verified=True, conditions: %#v", conditions)
			g.Expect(conditionStatus(conditions, "Available")).To(Equal("True"),
				"expected Available=True, conditions: %#v", conditions)
		}, timeout, interval).Should(Succeed())
	})
})

// conditionStatus returns the status ("True"/"False"/"Unknown") of the named
// condition in an unstructured status.conditions slice, or "" when the
// condition is absent.
func conditionStatus(conditions []interface{}, condType string) string {
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == condType {
			status, _ := cond["status"].(string)
			return status
		}
	}
	return ""
}

// newMCPServer builds a minimal v1alpha1 MCPServer. When ingressFrom is non-nil
// it is set at spec.network.ingressFrom; otherwise no network section is added,
// which exercises the restricted deny-by-default path.
func newMCPServer(namespace, name string, ingressFrom []interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"source": map[string]interface{}{
			"type": "ContainerImage",
			"containerImage": map[string]interface{}{
				"ref": "quay.io/containers/kubernetes_mcp_server:latest",
			},
		},
		"config": map[string]interface{}{"port": serverPortValue},
	}
	if ingressFrom != nil {
		spec["network"] = map[string]interface{}{"ingressFrom": ingressFrom}
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "mcp.x-k8s.io/v1alpha1",
		"kind":       "MCPServer",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}}
}

// serverPortValue is the MCP server port used by the posture specs.
const serverPortValue = int64(8080)
