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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"
	"github.com/opendatahub-io/odh-platform-utilities/pkg/controller/gc"
	"github.com/opendatahub-io/odh-platform-utilities/pkg/deploy"
	odhLabels "github.com/opendatahub-io/odh-platform-utilities/pkg/metadata/labels"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	"github.com/opendatahub-io/mcp-lifecycle-module-operator/internal/manifests"
)

// MCPLifecycleOperatorReconciler reconciles a MCPLifecycleOperator object.
type MCPLifecycleOperatorReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Deployer         *deploy.Deployer
	DynamicClient    dynamic.Interface
	DiscoveryClient  discovery.DiscoveryInterface
	ManifestProvider manifests.Provider
	OperatorVersion  string
	PodNamespace     string
}

const (
	platformConfigMapName = "opendatahub-mcplifecycleoperator-config"
	defaultRequeueDelay   = 10 * time.Second
)

// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=mcplifecycleoperators,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=mcplifecycleoperators/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=mcplifecycleoperators/finalizers,verbs=update
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcp.x-k8s.io,resources=mcpservers,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=mcp.x-k8s.io,resources=mcpservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=mcp.x-k8s.io,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:urls=/metrics,verbs=get

func (r *MCPLifecycleOperatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cr := &v1alpha1.MCPLifecycleOperator{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if k8serr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	orig := cr.DeepCopy()

	result, reconcileErr := r.reconcile(ctx, cr)

	if patchErr := r.patchStatus(ctx, orig, cr); patchErr != nil {
		log.Error(patchErr, "Failed to patch MCPLifecycleOperator status")
		if reconcileErr != nil {
			return ctrl.Result{}, errors.Join(reconcileErr, patchErr)
		}
		return ctrl.Result{}, patchErr
	}

	return result, reconcileErr
}

func (r *MCPLifecycleOperatorReconciler) reconcile(ctx context.Context, cr *v1alpha1.MCPLifecycleOperator) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	defer func() {
		cr.Status.Status.ObservedGeneration = cr.Generation
		cr.Status.Status.Phase = cm.Phase()
		cr.SetReleaseStatus(platformcommon.ComponentReleaseStatus{
			Releases: []platformcommon.ComponentRelease{{
				Name:    v1alpha1.MCPLifecycleOperatorServiceName,
				RepoURL: "https://github.com/opendatahub-io/mcp-lifecycle-module-operator",
				Version: r.OperatorVersion,
			}},
		})
	}()

	if cr.Spec.ManagementState == platformcommon.Removed {
		return r.handleRemoved(ctx, cr, cm)
	}

	operandImage, operandNamespace, err := r.readPlatformConfig(ctx)
	if err != nil {
		cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable,
			"ConfigReadFailed", fmt.Sprintf("Platform config not available: %v", err))
		cm.AggregateReady()
		return ctrl.Result{}, fmt.Errorf("reading platform config: %w", err)
	}

	desired, err := r.ManifestProvider.Manifests(ctx, manifests.Params{
		OperandNamespace: operandNamespace,
		OperandImage:     operandImage,
	})
	if err != nil {
		cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable,
			"ManifestRenderFailed", fmt.Sprintf("Failed to render operand manifests: %v", err))
		cm.AggregateReady()
		return ctrl.Result{}, fmt.Errorf("rendering operand manifests: %w", err)
	}

	if err := r.applyResources(ctx, cr, desired); err != nil {
		cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable,
			"DeployFailed", fmt.Sprintf("Failed to apply operand resources: %v", err))
		cm.AggregateReady()
		return ctrl.Result{}, fmt.Errorf("applying operand resources: %w", err)
	}

	if err := r.collectGarbage(ctx, cr, operandNamespace, desired); err != nil {
		cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable,
			"GarbageCollectionFailed", fmt.Sprintf("Failed to collect garbage: %v", err))
		cm.AggregateReady()
		log.Error(err, "Garbage collection encountered errors")
		return ctrl.Result{}, fmt.Errorf("collecting garbage: %w", err)
	}

	if result, ready := r.checkDeploymentsReady(ctx, desired, cm); !ready {
		return result, nil
	}

	cm.MarkTrue(v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	cm.AggregateReady()

	return ctrl.Result{}, nil
}

func (r *MCPLifecycleOperatorReconciler) handleRemoved(ctx context.Context, cr *v1alpha1.MCPLifecycleOperator, cm *v1alpha1.ConditionsManager) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("ManagementState is Removed, deleting all owned resources")
	if err := r.deleteAllOwned(ctx, cr); err != nil {
		log.Error(err, "Failed to delete owned resources, will retry on next reconcile")
		return ctrl.Result{RequeueAfter: defaultRequeueDelay}, fmt.Errorf("deleting owned resources: %w", err)
	}

	cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable, "Removed", "MCPLifecycleOperator is in Removed state")
	cm.MarkFalse(string(platformcommon.ConditionTypeReady), "Removed", "MCPLifecycleOperator is in Removed state")
	cm.MarkFalse(string(platformcommon.ConditionTypeProvisioningSucceeded), "Removed", "MCPLifecycleOperator is in Removed state")
	cm.MarkFalse(string(platformcommon.ConditionTypeDegraded), "NotDegraded", "")

	return ctrl.Result{}, nil
}

func (r *MCPLifecycleOperatorReconciler) readPlatformConfig(ctx context.Context) (image, namespace string, err error) {
	configMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.PodNamespace,
		Name:      platformConfigMapName,
	}, configMap); err != nil {
		return "", "", err
	}

	image = configMap.Data["operand-image"]
	if image == "" {
		return "", "", fmt.Errorf("key %q is missing or empty in ConfigMap %s/%s",
			"operand-image", r.PodNamespace, platformConfigMapName)
	}

	namespace = configMap.Data["operand-namespace"]
	if namespace == "" {
		namespace = manifests.DefaultOperandNamespace
	}

	return image, namespace, nil
}

func (r *MCPLifecycleOperatorReconciler) applyResources(ctx context.Context, cr *v1alpha1.MCPLifecycleOperator, desired []unstructured.Unstructured) error {
	return r.Deployer.Deploy(ctx, deploy.DeployInput{
		Client:    r.Client,
		Owner:     cr,
		Release:   deploy.ReleaseInfo{Type: "OpenDataHub", Version: r.OperatorVersion},
		Resources: desired,
	})
}

func (r *MCPLifecycleOperatorReconciler) checkDeploymentsReady(ctx context.Context, desired []unstructured.Unstructured, cm *v1alpha1.ConditionsManager) (ctrl.Result, bool) {
	for _, dn := range findDeploymentNames(desired) {
		operandDeployment := &appsv1.Deployment{}
		if err := r.Get(ctx, dn, operandDeployment); err != nil {
			cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable,
				"DeploymentNotFound", fmt.Sprintf("Operand deployment %s not found: %v", dn.Name, err))
			cm.AggregateReady()
			return ctrl.Result{RequeueAfter: defaultRequeueDelay}, false
		}

		desiredReplicas := int32(1)
		if operandDeployment.Spec.Replicas != nil {
			desiredReplicas = *operandDeployment.Spec.Replicas
		}
		if operandDeployment.Status.AvailableReplicas < desiredReplicas {
			msg := fmt.Sprintf("Operand deployment %s has %d/%d available replicas",
				dn.Name, operandDeployment.Status.AvailableReplicas, desiredReplicas)
			// Prefer ReplicaFailure over Available as it carries actionable diagnostics.
			// K8s does not guarantee condition ordering.
			for _, c := range operandDeployment.Status.Conditions {
				if c.Type == appsv1.DeploymentReplicaFailure && c.Message != "" {
					msg = c.Message
					break
				}
				if c.Type == appsv1.DeploymentAvailable && c.Message != "" {
					msg = c.Message
				}
			}
			cm.MarkFalse(v1alpha1.ConditionMCPLifecycleOperatorAvailable, "DeploymentNotReady", msg)
			cm.AggregateReady()
			return ctrl.Result{RequeueAfter: defaultRequeueDelay}, false
		}
	}

	return ctrl.Result{}, true
}

func findDeploymentNames(resources []unstructured.Unstructured) []types.NamespacedName {
	var names []types.NamespacedName
	for i := range resources {
		obj := &resources[i]
		if obj.GetKind() == "Deployment" {
			names = append(names, types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			})
		}
	}
	return names
}

type resourceKey struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

func (r *MCPLifecycleOperatorReconciler) collectGarbage(ctx context.Context, cr *v1alpha1.MCPLifecycleOperator, operandNamespace string, desired []unstructured.Unstructured) error {
	desiredSet := make(map[resourceKey]struct{}, len(desired))
	for i := range desired {
		obj := &desired[i]
		desiredSet[resourceKey{
			gvk:       obj.GroupVersionKind(),
			namespace: obj.GetNamespace(),
			name:      obj.GetName(),
		}] = struct{}{}
	}

	collector := gc.New(
		gc.WithOnlyCollectOwned(false),
		gc.WithLabel(odhLabels.PlatformPartOf, v1alpha1.MCPLifecycleOperatorServiceName),
		gc.InNamespace(operandNamespace),
		gc.WithObjectPredicate(func(_ gc.RunParams, obj unstructured.Unstructured) (bool, error) {
			k := resourceKey{
				gvk:       obj.GroupVersionKind(),
				namespace: obj.GetNamespace(),
				name:      obj.GetName(),
			}
			_, inDesired := desiredSet[k]
			return !inDesired, nil
		}),
	)

	return collector.Run(ctx, gc.RunParams{
		Client:          r.Client,
		DynamicClient:   r.DynamicClient,
		DiscoveryClient: r.DiscoveryClient,
		Owner:           cr,
		Version:         r.OperatorVersion,
		PlatformType:    "OpenDataHub",
	})
}

func (r *MCPLifecycleOperatorReconciler) deleteAllOwned(ctx context.Context, cr *v1alpha1.MCPLifecycleOperator) error {
	operandNamespace := r.resolveOperandNamespace(ctx)

	collector := gc.New(
		gc.WithOnlyCollectOwned(false),
		gc.WithLabel(odhLabels.PlatformPartOf, v1alpha1.MCPLifecycleOperatorServiceName),
		gc.InNamespace(operandNamespace),
		gc.WithObjectPredicate(func(_ gc.RunParams, _ unstructured.Unstructured) (bool, error) {
			return true, nil
		}),
	)

	return collector.Run(ctx, gc.RunParams{
		Client:          r.Client,
		DynamicClient:   r.DynamicClient,
		DiscoveryClient: r.DiscoveryClient,
		Owner:           cr,
		Version:         r.OperatorVersion,
		PlatformType:    "OpenDataHub",
	})
}

func (r *MCPLifecycleOperatorReconciler) resolveOperandNamespace(ctx context.Context) string {
	log := logf.FromContext(ctx)

	_, ns, err := r.readPlatformConfig(ctx)
	if err != nil {
		log.Info("Platform ConfigMap not available, falling back to default operand namespace",
			"default", manifests.DefaultOperandNamespace, "error", err)
		return manifests.DefaultOperandNamespace
	}
	return ns
}

func (r *MCPLifecycleOperatorReconciler) patchStatus(ctx context.Context, orig, updated *v1alpha1.MCPLifecycleOperator) error {
	patch := client.MergeFrom(orig)
	return r.Status().Patch(ctx, updated, patch)
}

// SetupWithManager registers the controller with the manager.
func (r *MCPLifecycleOperatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toSingleton := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}},
		}
	})

	managedPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[odhLabels.PlatformPartOf] == v1alpha1.MCPLifecycleOperatorServiceName
	})

	configMapPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.PodNamespace && obj.GetName() == platformConfigMapName
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MCPLifecycleOperator{}).
		Watches(&corev1.ConfigMap{}, toSingleton, builder.WithPredicates(configMapPredicate)).
		Watches(&appsv1.Deployment{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&corev1.ServiceAccount{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&corev1.Service{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&rbacv1.ClusterRole{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&rbacv1.ClusterRoleBinding{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&rbacv1.Role{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&rbacv1.RoleBinding{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Watches(&extv1.CustomResourceDefinition{}, toSingleton, builder.WithPredicates(managedPredicate)).
		Complete(r)
}
