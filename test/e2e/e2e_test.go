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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

var (
	_ client.Object = &v1alpha1.MCPLifecycleOperator{}
)

const (
	operandNamespace         = "mcp-lifecycle-module-operator-system"
	operandDeployment        = "mcp-lifecycle-operator-controller-manager"
	operandCRD               = "mcpservers.mcp.x-k8s.io"
	moduleOperatorDeployment = "mcp-lifecycle-module-operator-controller-manager"

	timeout            = 5 * time.Minute
	interval           = 5 * time.Second
	consistentDuration = 30 * time.Second
	consistentInterval = 5 * time.Second
)

var _ = Describe("MCPLifecycleOperator", func() {
	ctx := context.Background()

	AfterEach(func() {
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			},
		}
		err := k8sClient.Delete(ctx, cr)
		if err != nil && !k8serr.IsNotFound(err) {
			Fail("failed to delete MCPLifecycleOperator CR: " + err.Error())
		}
	})

	It("should deploy the MCP Lifecycle Operator when the CR is created", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)
	})

	It("should reject an MCPLifecycleOperator CR whose name is not default", func() {
		By("Creating the MCPLifecycleOperator CR with a non-default name")
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name: "not-default",
			},
			Spec: v1alpha1.MCPLifecycleOperatorSpec{
				ManagementSpec: platformcommon.ManagementSpec{
					ManagementState: platformcommon.Managed,
				},
			},
		}

		// Guard against a CEL regression: if the API server accepts the
		// non-default CR, ensure it is cleaned up so it does not leak into
		// later specs (AfterEach only deletes the "default" singleton).
		DeferCleanup(func() {
			err := k8sClient.Delete(ctx, cr)
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
		createManagedCR(ctx)
		waitForOperandReady(ctx)

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

	It("should keep namespace and module operator when CR is deleted", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Deleting the MCPLifecycleOperator CR")
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			},
		}
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

		By("Waiting for the CR to be gone")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, &v1alpha1.MCPLifecycleOperator{})
			g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		By("Verifying the namespace still exists")
		Consistently(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
		}, consistentDuration, consistentInterval).Should(Succeed())

		By("Verifying the module operator deployment is still available")
		Consistently(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      moduleOperatorDeployment,
			}, dep)).To(Succeed())
			g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
		}, consistentDuration, consistentInterval).Should(Succeed())
	})

	It("should keep namespace and module operator when ManagementState is set to Removed", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Setting ManagementState to Removed")
		cr := &v1alpha1.MCPLifecycleOperator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		cr.Spec.ManagementState = platformcommon.Removed
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		By("Waiting for the operand deployment to be removed")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      operandDeployment,
			}, &appsv1.Deployment{})
			g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		By("Verifying the namespace still exists")
		Consistently(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
		}, consistentDuration, consistentInterval).Should(Succeed())

		By("Verifying the module operator deployment is still available")
		Consistently(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      moduleOperatorDeployment,
			}, dep)).To(Succeed())
			g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
		}, consistentDuration, consistentInterval).Should(Succeed())
	})

	It("should update observedGeneration to match generation after a spec change", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Recording the current generation of the CR")
		cr := &v1alpha1.MCPLifecycleOperator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		genBefore := cr.Generation

		By("Changing ManagementState to trigger a spec update")
		Eventually(func(g Gomega) {
			fresh := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, fresh)).To(Succeed())
			fresh.Spec.ManagementState = platformcommon.Removed
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		By("Verifying observedGeneration catches up to the new generation")
		Eventually(func(g Gomega) {
			updated := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, updated)).To(Succeed())

			g.Expect(updated.Generation).To(BeNumerically(">", genBefore))
			g.Expect(updated.Status.Status.ObservedGeneration).To(Equal(updated.Generation))
		}, timeout, interval).Should(Succeed())
	})
})

func createManagedCR(ctx context.Context) {
	By("Creating the MCPLifecycleOperator CR")
	cr := &v1alpha1.MCPLifecycleOperator{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		},
		Spec: v1alpha1.MCPLifecycleOperatorSpec{
			ManagementSpec: platformcommon.ManagementSpec{
				ManagementState: platformcommon.Managed,
			},
		},
	}
	Expect(k8sClient.Create(ctx, cr)).To(Succeed())
}

func waitForOperandReady(ctx context.Context) {
	By("Waiting for the operand namespace to be created")
	Eventually(func(g Gomega) {
		ns := &corev1.Namespace{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand CRD to be installed")
	Eventually(func(g Gomega) {
		crd := &extv1.CustomResourceDefinition{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand deployment to become available")
	Eventually(func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: operandNamespace,
			Name:      operandDeployment,
		}, dep)).To(Succeed())

		desiredReplicas := int32(1)
		if dep.Spec.Replicas != nil {
			desiredReplicas = *dep.Spec.Replicas
		}
		g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", desiredReplicas))
	}, timeout, interval).Should(Succeed())

	By("Verifying the CR reaches Ready=True")
	Eventually(func(g Gomega) {
		updated := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, updated)).To(Succeed())

		readyCondition := findCondition(updated.Status.Conditions, string(platformcommon.ConditionTypeReady))
		g.Expect(readyCondition).NotTo(BeNil())
		g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
	}, timeout, interval).Should(Succeed())
}

func findRelease(releases []platformcommon.ComponentRelease, name string) *platformcommon.ComponentRelease {
	for i := range releases {
		if releases[i].Name == name {
			return &releases[i]
		}
	}
	return nil
}

func findCondition(conditions []platformcommon.Condition, condType string) *platformcommon.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
