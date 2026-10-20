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
	"fmt"
	"net/http"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

var _ = Describe("MCPLifecycleOperator installation", func() {
	ctx := context.Background()

	BeforeEach(prepareTest)

	It("installs a reachable operand webhook with fail-closed admission registration", func() {
		const serviceName = "mcp-lifecycle-operator-webhook-service"
		By("Checking the webhook Service and fail-closed admission registration")
		Eventually(func(g Gomega) {
			service := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: serviceName}, service)).To(Succeed())
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-operator"))

			module := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, module)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(module.Spec.Template.Labels))).To(BeFalse(),
				"webhook Service selector %v matches module Pod labels %v", service.Spec.Selector, module.Spec.Template.Labels)

			configuration := &admissionv1.ValidatingWebhookConfiguration{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mcp-lifecycle-operator-validating-webhook-configuration"}, configuration)).To(Succeed())
			index := slices.IndexFunc(configuration.Webhooks, func(webhook admissionv1.ValidatingWebhook) bool {
				return webhook.Name == "vmcpserver.mcp.x-k8s.io"
			})
			g.Expect(index).To(BeNumerically(">=", 0))
			webhook := configuration.Webhooks[index]
			g.Expect(webhook.ClientConfig.Service).To(HaveValue(SatisfyAll(
				HaveField("Name", serviceName),
				HaveField("Namespace", operandNamespace),
			)))
			g.Expect(webhook.FailurePolicy).To(HaveValue(Equal(admissionv1.Fail)))
			g.Expect(webhook.NamespaceSelector).To(Or(BeNil(), Equal(&metav1.LabelSelector{})))
			g.Expect(webhook.ObjectSelector).To(Or(BeNil(), Equal(&metav1.LabelSelector{})))
			g.Expect(webhook.MatchConditions).To(BeEmpty())
			g.Expect(webhook.Rules).To(ContainElement(SatisfyAll(
				HaveField("Operations", ContainElement(admissionv1.Create)),
				HaveField("APIGroups", ContainElement("mcp.x-k8s.io")),
				HaveField("APIVersions", ContainElement("v1alpha1")),
				HaveField("Resources", ContainElement("mcpservers")),
				HaveField("Scope", Or(BeNil(), HaveValue(BeElementOf(admissionv1.AllScopes, admissionv1.NamespacedScope)))),
			)))
		}, timeout, interval).Should(Succeed())

		By("Sending a dry-run MCPServer create request through admission")
		server := newMCPServer(operandNamespace, "mcplmo-e2e-webhook", nil)
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Create(ctx, server.DeepCopy(), client.DryRunAll)).To(Succeed())
		}, timeout, interval).Should(Succeed())
	})

	It("deploys the MCP Lifecycle Operator with a reachable metrics endpoint", func() {
		const metricsService = "mcp-lifecycle-operator-controller-manager-metrics-service"
		const metricsPort = int32(8443)

		By("Checking the metrics Service selects the operand manager Pod and not the module operator Pod")
		Eventually(func(g Gomega) {
			service := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: metricsService}, service)).To(Succeed())
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-operator"))
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("control-plane", "controller-manager"))
			g.Expect(service.Spec.Ports).To(ContainElement(SatisfyAll(
				HaveField("Name", "https"),
				HaveField("Port", metricsPort),
			)))

			operand := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, operand)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(operand.Spec.Template.Labels))).To(BeTrue(),
				"metrics Service selector %v does not match operand Pod labels %v", service.Spec.Selector, operand.Spec.Template.Labels)

			module := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, module)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(module.Spec.Template.Labels))).To(BeFalse(),
				"metrics Service selector %v matches module Pod labels %v", service.Spec.Selector, module.Spec.Template.Labels)
		}, timeout, interval).Should(Succeed())

		By("Verifying the metrics Service has an EndpointSlice backed by a ready operand Pod")
		Eventually(func(g Gomega) {
			endpointSlices := &discoveryv1.EndpointSliceList{}
			g.Expect(k8sClient.List(ctx, endpointSlices,
				client.InNamespace(operandNamespace),
				client.MatchingLabels{discoveryv1.LabelServiceName: metricsService},
			)).To(Succeed())

			readyAddresses := 0
			servesMetricsPort := false
			for _, slice := range endpointSlices.Items {
				for _, endpoint := range slice.Endpoints {
					// Ready is a *bool; nil conservatively means Ready per the API.
					if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
						readyAddresses += len(endpoint.Addresses)
					}
				}
				for _, port := range slice.Ports {
					if port.Port != nil && *port.Port == metricsPort {
						servesMetricsPort = true
					}
				}
			}
			g.Expect(readyAddresses).To(BeNumerically(">=", 1), "metrics Service %q has no ready endpoints", metricsService)
			g.Expect(servesMetricsPort).To(BeTrue(), "metrics Service %q endpoints do not expose port %d", metricsService, metricsPort)
		}, timeout, interval).Should(Succeed())

		By("Hitting the metrics endpoint through the API server Service proxy")
		clientset, err := kubernetes.NewForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			result := clientset.CoreV1().RESTClient().Get().
				Namespace(operandNamespace).
				Resource("services").
				Name(fmt.Sprintf("https:%s:%d", metricsService, metricsPort)).
				SubResource("proxy").
				Suffix("metrics").
				Do(ctx)
			var statusCode int
			result.StatusCode(&statusCode)
			reqErr := result.Error()
			reachable := statusCode == http.StatusOK ||
				statusCode == http.StatusUnauthorized ||
				k8serr.IsUnauthorized(reqErr)
			g.Expect(reachable).To(BeTrue(),
				"metrics endpoint not reachable: HTTP %d via Service proxy (request err: %v)", statusCode, reqErr)
		}, timeout, interval).Should(Succeed())
	})

	It("should reject an MCPLifecycleOperator CR whose name is not default", func() {
		By("Creating the MCPLifecycleOperator CR with a non-default name")
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "mcplmo-e2e-invalid-",
			},
			Spec: v1alpha1.MCPLifecycleOperatorSpec{
				ManagementSpec: platformcommon.ManagementSpec{
					ManagementState: platformcommon.Managed,
				},
			},
		}

		DeferCleanup(func() {
			if cr.UID == "" {
				return
			}
			uid := cr.UID
			err := k8sClient.Delete(ctx, cr, client.Preconditions{UID: &uid})
			if err != nil && !k8serr.IsNotFound(err) {
				Fail("failed to delete non-default MCPLifecycleOperator CR: " + err.Error())
			}
		})

		By("Verifying the API server rejects the CR via the singleton CEL rule")
		err := k8sClient.Create(ctx, cr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must be default"))
	})

	It("should report the module release metadata in status.releases", func() {
		By("Verifying status.releases contains the module release metadata")
		Eventually(func(g Gomega) {
			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, cr)).To(Succeed())

			release := findRelease(cr.Status.Releases, v1alpha1.MCPLifecycleOperatorServiceName)
			g.Expect(release).NotTo(BeNil())
			g.Expect(release.RepoURL).To(Equal("https://github.com/opendatahub-io/mcp-lifecycle-module-operator"))
			g.Expect(release.Version).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())
	})

	It("injects the restricted posture flag into the operand controller-manager", func() {
		const postureFlag = "--network-policy-default-posture"
		const restricted = "restricted"
		const managerName = "manager"

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: operandNamespace,
			Name:      operandDeployment,
		}, dep)).To(Succeed())

		var manager *corev1.Container
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == managerName {
				manager = &dep.Spec.Template.Spec.Containers[i]
				break
			}
		}
		Expect(manager).NotTo(BeNil(), "operand Deployment has no %q container", managerName)
		Expect(manager.Args).To(ContainElement(postureFlag+"="+restricted),
			"operand manager args %v missing %s=%s", manager.Args, postureFlag, restricted)
	})
})

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

func findRelease(releases []platformcommon.ComponentRelease, name string) *platformcommon.ComponentRelease {
	for i := range releases {
		if releases[i].Name == name {
			return &releases[i]
		}
	}
	return nil
}
