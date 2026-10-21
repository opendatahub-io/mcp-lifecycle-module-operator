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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

// TestE2ELifecycleSingletonDeletion verifies singleton deletion garbage-collects the operand and preserves the module installation.
func TestE2ELifecycleSingletonDeletion(t *testing.T) {
	prepareSuite(t)
	requireUnownedSingleton(t)
	g := NewWithT(t)
	ctx := context.Background()
	waitForOperandReady(t, ctx)

	t.Log("Recording the singleton and its owned operand")
	module := &v1alpha1.MCPLifecycleOperator{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, module)).To(Succeed())

	uid := module.UID
	t.Cleanup(func() {
		err := k8sClient.Delete(ctx, module, client.Preconditions{UID: &uid})
		g.Expect(err == nil || k8serr.IsNotFound(err)).To(BeTrue(), "delete old singleton: %v", err)
		g.Eventually(func() error { return checkSingletonDeletion(ctx) }, timeout, interval).Should(Succeed())
		createManagedSingleton(t, ctx)
	})
	t.Log("Verifying the operand Deployment is controlled by the singleton")
	operand := &appsv1.Deployment{}
	g.Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, operand)).To(Succeed())
		g.Expect(operand.OwnerReferences).To(ContainElement(SatisfyAll(
			HaveField("Kind", "MCPLifecycleOperator"),
			HaveField("UID", uid),
			HaveField("Controller", HaveValue(BeTrue())),
		)))
	}, timeout, interval).Should(Succeed())
	t.Log("Deleting the MCPLifecycleOperator singleton")
	g.Expect(k8sClient.Delete(ctx, module, client.Preconditions{UID: &uid})).To(Succeed())

	t.Log("Waiting for singleton absence and operand garbage collection")
	g.Eventually(func() error {
		return checkSingletonDeletion(ctx)
	}, timeout, interval).Should(Succeed())

	t.Log("Verifying the namespace and module operator remain available")
	g.Consistently(func(g Gomega) {
		g.Expect(checkSingletonDeletion(ctx)).To(Succeed())
		assertModuleInstallationRemains(g, ctx)
	}, consistentDuration, consistentInterval).Should(Succeed())
}

// TestE2ELifecycleRemovedReinstall verifies Removed is acknowledged and an MCPServer survives reinstallation.
func TestE2ELifecycleRemovedReinstall(t *testing.T) {
	prepareTest(t)
	g := NewWithT(t)
	ctx := context.Background()

	t.Log("Creating an MCPServer whose identity and spec must survive reinstallation")
	fixtureNamespace := createTestNamespace(t, ctx, "mcplmo-e2e-retention-")
	server := newMCPServer(fixtureNamespace, "retained", nil)
	server.SetAPIVersion("mcp.x-k8s.io/v1beta1")
	g.Expect(unstructured.SetNestedField(server.Object, int64(0), "spec", "runtime", "replicas")).To(Succeed())
	g.Eventually(func(g Gomega) {
		err := k8sClient.Create(ctx, server.DeepCopy())
		g.Expect(err == nil || k8serr.IsAlreadyExists(err)).To(BeTrue(), "create retention fixture: %v", err)
	}, timeout, interval).Should(Succeed())
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(server), server)).To(Succeed())

	t.Log("Recording the current singleton identity and generation")
	cr := &v1alpha1.MCPLifecycleOperator{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, cr)).To(Succeed())
	uid, generation := cr.UID, cr.Generation
	t.Cleanup(func() { restoreManagedSingleton(t, ctx, uid) })

	t.Log("Setting ManagementState to Removed, retrying only API conflicts")
	g.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &v1alpha1.MCPLifecycleOperator{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
			return err
		}
		g.Expect(fresh.UID).To(Equal(uid), "singleton was replaced before the state change")
		original := fresh.DeepCopy()
		fresh.Spec.ManagementState = platformcommon.Removed
		if err := k8sClient.Patch(ctx, fresh, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		cr = fresh
		return nil
	})).To(Succeed())
	g.Expect(cr.Generation).To(BeNumerically(">", generation))

	t.Log("Waiting for acknowledgment of the requested generation and operand removal")
	g.Eventually(func(g Gomega) {
		current := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), current)).To(Succeed())
		g.Expect(current.UID).To(Equal(uid))
		g.Expect(current.Spec.ManagementState).To(Equal(platformcommon.Removed))
		g.Expect(current.Generation).To(Equal(cr.Generation))
		g.Expect(current.Status.Status.ObservedGeneration).To(Equal(cr.Generation))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, &appsv1.Deployment{})
		g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
	}, timeout, interval).Should(Succeed())

	t.Log("Verifying Removed persists while the namespace and module operator remain available")
	g.Consistently(func(g Gomega) {
		current := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), current)).To(Succeed())
		g.Expect(current.UID).To(Equal(uid))
		g.Expect(current.Spec.ManagementState).To(Equal(platformcommon.Removed))
		g.Expect(current.Generation).To(Equal(cr.Generation))
		g.Expect(current.Status.Status.ObservedGeneration).To(Equal(cr.Generation))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, &appsv1.Deployment{})
		g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		assertModuleInstallationRemains(g, ctx)
	}, consistentDuration, consistentInterval).Should(Succeed())

	t.Log("Reinstalling the operand and verifying the original MCPServer and its spec remain")
	restoreManagedSingleton(t, ctx, uid)
	g.Eventually(func(g Gomega) {
		retained := &unstructured.Unstructured{}
		retained.SetGroupVersionKind(server.GroupVersionKind())
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(server), retained)).To(Succeed())
		g.Expect(retained.GetUID()).To(Equal(server.GetUID()))
		g.Expect(retained.GetDeletionTimestamp()).To(BeNil())
		expectMCPServerSpec(g, retained, serverPortValue)

		image, found, err := unstructured.NestedString(retained.Object, "spec", "source", "containerImage", "ref")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(image).To(Equal("quay.io/containers/kubernetes_mcp_server:latest"))

		replicas, found, err := unstructured.NestedInt64(retained.Object, "spec", "runtime", "replicas")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(replicas).To(Equal(int64(0)))
	}, timeout, interval).Should(Succeed())
}

// TestE2ELifecycleOperandOutage verifies the module reports Ready=False during an operand outage and recovers.
func TestE2ELifecycleOperandOutage(t *testing.T) {
	prepareTest(t)
	g := NewWithT(t)
	ctx := context.Background()

	t.Log("Finding the operand Pod")
	pods := &corev1.PodList{}
	g.Eventually(func(g Gomega) {
		g.Expect(k8sClient.List(ctx, pods,
			client.InNamespace(operandNamespace),
			client.MatchingLabels{"app.kubernetes.io/name": "mcp-lifecycle-operator", "control-plane": "controller-manager"},
		)).To(Succeed())
		g.Expect(pods.Items).To(HaveLen(1))
		g.Expect(pods.Items[0].DeletionTimestamp).To(BeNil())
	}, timeout, interval).Should(Succeed())
	pod := pods.Items[0].DeepCopy()
	g.Expect(pod.Spec.Containers).To(HaveLen(1))
	g.Expect(pod.Spec.Containers[0].Name).To(Equal("manager"))
	uid := pod.UID
	t.Cleanup(func() {
		err := k8sClient.Delete(ctx, pod, client.Preconditions{UID: &uid})
		g.Expect(err == nil || k8serr.IsNotFound(err)).To(BeTrue(), "delete patched operand Pod: %v", err)
		waitForOperandReady(t, ctx)
	})

	t.Log("Making only the running operand Pod unpullable")
	original := pod.DeepCopy()
	pod.Spec.Containers[0].Image = "registry.invalid/mcplmo-e2e/unpullable:latest"
	g.Expect(k8sClient.Patch(ctx, pod, client.MergeFrom(original))).To(Succeed())

	t.Log("Verifying the operator reports the unavailable operand")
	g.Eventually(func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, dep)).To(Succeed())
		g.Expect(dep.Status.AvailableReplicas).To(Equal(int32(0)))

		cr := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, cr)).To(Succeed())
		g.Expect(cr.Status.Conditions).To(ContainElements(
			SatisfyAll(
				HaveField("Type", string(v1alpha1.ConditionMCPLifecycleOperatorAvailable)),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "DeploymentNotReady"),
			),
			SatisfyAll(
				HaveField("Type", string(platformcommon.ConditionTypeReady)),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "OperandDeploymentFailed"),
				HaveField("ObservedGeneration", cr.Generation),
			),
			SatisfyAll(
				HaveField("Type", string(platformcommon.ConditionTypeDegraded)),
				HaveField("Status", metav1.ConditionFalse),
			),
		))
	}, timeout, interval).Should(Succeed())
}

func assertModuleInstallationRemains(g Gomega, ctx context.Context) {
	ns := &corev1.Namespace{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
	dep := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: operandNamespace,
		Name:      moduleOperatorDeployment,
	}, dep)).To(Succeed())
	g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
}
