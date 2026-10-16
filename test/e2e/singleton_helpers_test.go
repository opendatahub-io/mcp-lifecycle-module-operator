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

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"
)

func createManagedSingleton(ctx context.Context) {
	cr := &v1alpha1.MCPLifecycleOperator{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
		Spec: v1alpha1.MCPLifecycleOperatorSpec{
			ManagementSpec: platformcommon.ManagementSpec{ManagementState: platformcommon.Managed},
		},
	}
	Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	waitForOperandReady(ctx)
}

func restoreManagedSingleton(ctx context.Context, uid types.UID) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cr := &v1alpha1.MCPLifecycleOperator{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, cr); err != nil {
			return err
		}
		if cr.UID != uid || cr.DeletionTimestamp != nil {
			return fmt.Errorf("singleton was replaced or deleted; refusing to overwrite it")
		}
		if cr.Spec.ManagementState == platformcommon.Managed {
			return nil
		}
		original := cr.DeepCopy()
		cr.Spec.ManagementState = platformcommon.Managed
		return k8sClient.Patch(ctx, cr, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	})).To(Succeed(), "restoring the singleton to Managed")
	waitForOperandReady(ctx)
}

func checkReadiness(ctx context.Context) error {
	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, deployment); err != nil {
		return err
	}
	cr := &v1alpha1.MCPLifecycleOperator{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, cr); err != nil {
		return err
	}
	if cr.Spec.ManagementState != platformcommon.Managed || cr.Status.Status.ObservedGeneration != cr.Generation {
		return fmt.Errorf("module has not acknowledged Managed generation %d", cr.Generation)
	}
	if !deploymentReady(deployment) {
		return fmt.Errorf("operand Deployment has not completed its rollout")
	}
	for _, name := range []string{"Ready", "ProvisioningSucceeded"} {
		found := false
		for _, condition := range cr.Status.Conditions {
			if condition.Type == name && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == cr.Generation {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("module %s is not %s for generation %d", name, metav1.ConditionTrue, cr.Generation)
		}
	}
	return nil
}

func deploymentReady(deployment *appsv1.Deployment) bool {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return desired > 0 && deployment.DeletionTimestamp == nil && deployment.Status.ObservedGeneration == deployment.Generation &&
		deployment.Status.UpdatedReplicas == desired && deployment.Status.ReadyReplicas == desired && deployment.Status.AvailableReplicas == desired
}

func checkSingletonDeletion(ctx context.Context) error {
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, &v1alpha1.MCPLifecycleOperator{})
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("singleton remains or could not be read: %v", err)
	}
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("owned operand Deployment has not been garbage-collected or could not be read: %v", err)
	}
	return nil
}
