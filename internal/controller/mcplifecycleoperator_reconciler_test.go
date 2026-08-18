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
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"
	"github.com/opendatahub-io/odh-platform-utilities/pkg/deploy"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	"github.com/opendatahub-io/mcp-lifecycle-module-operator/internal/manifests"
)

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(appsv1.AddToScheme(s))
	utilruntime.Must(authorizationv1.AddToScheme(s))
	utilruntime.Must(configv1.Install(s))
	return s
}()

const (
	testPodNamespace    = "operator-ns"
	testOperatorVersion = "v0.1.0-test"
	testOperandImage    = "registry.io/image:v1"
)

// --- Fake manifest provider ---

type fakeManifestProvider struct {
	resources []unstructured.Unstructured
	err       error
}

func (f *fakeManifestProvider) Manifests(_ context.Context, _ manifests.Params) ([]unstructured.Unstructured, error) {
	return f.resources, f.err
}

type capturingManifestProvider struct {
	delegate manifests.Provider
	capture  func(manifests.Params)
}

func (c *capturingManifestProvider) Manifests(ctx context.Context, params manifests.Params) ([]unstructured.Unstructured, error) {
	c.capture(params)
	return c.delegate.Manifests(ctx, params)
}

// --- Test helpers ---

func newTestReconciler(cli client.Client, provider manifests.Provider, operandImage string) *MCPLifecycleOperatorReconciler {
	return &MCPLifecycleOperatorReconciler{
		Client:           cli,
		Scheme:           testScheme,
		ManifestProvider: provider,
		OperatorVersion:  testOperatorVersion,
		PodNamespace:     testPodNamespace,
		OperandImage:     operandImage,
	}
}

func newTestReconcilerFull(provider manifests.Provider, operandImage string, objects ...client.Object) *MCPLifecycleOperatorReconciler {
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.MCPLifecycleOperator{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*authorizationv1.SelfSubjectRulesReview); ok {
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	kubeClient := kubefake.NewSimpleClientset()

	return &MCPLifecycleOperatorReconciler{
		Client:           cli,
		Scheme:           testScheme,
		Deployer:         deploy.NewDeployer(),
		DynamicClient:    fakedynamic.NewSimpleDynamicClient(testScheme),
		DiscoveryClient:  kubeClient.Discovery(),
		ManifestProvider: provider,
		OperatorVersion:  testOperatorVersion,
		PodNamespace:     testPodNamespace,
		OperandImage:     operandImage,
	}
}

func newTestCR() *v1alpha1.MCPLifecycleOperator {
	return &v1alpha1.MCPLifecycleOperator{
		ObjectMeta: metav1.ObjectMeta{
			Name:       v1alpha1.MCPLifecycleOperatorInstanceName,
			Generation: 1,
		},
		Spec: v1alpha1.MCPLifecycleOperatorSpec{
			ManagementSpec: platformcommon.ManagementSpec{
				ManagementState: platformcommon.Managed,
			},
		},
	}
}

func findCondition(cr *v1alpha1.MCPLifecycleOperator, condType string) *platformcommon.Condition {
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == condType {
			return &cr.Status.Conditions[i]
		}
	}
	return nil
}

func newDeploymentUnstructured(name, namespace string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
}

func int32Ptr(i int32) *int32 { return &i }

// --- findDeploymentNames tests ---

func TestFindDeploymentNames(t *testing.T) {
	resources := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata":   map[string]interface{}{"name": "sa", "namespace": "ns"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "controller-manager", "namespace": "target-ns"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "webhook", "namespace": "target-ns"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata":   map[string]interface{}{"name": "manager-role"},
		}},
	}

	names := findDeploymentNames(resources)

	if len(names) != 2 {
		t.Fatalf("expected 2 deployments, got %d", len(names))
	}

	expected := []types.NamespacedName{
		{Namespace: "target-ns", Name: "controller-manager"},
		{Namespace: "target-ns", Name: "webhook"},
	}
	for i, nn := range names {
		if nn != expected[i] {
			t.Errorf("deployment[%d] = %v, want %v", i, nn, expected[i])
		}
	}
}

func TestFindDeploymentNamesEmpty(t *testing.T) {
	resources := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "cm", "namespace": "ns"},
		}},
	}

	names := findDeploymentNames(resources)
	if len(names) != 0 {
		t.Fatalf("expected 0 deployments, got %d", len(names))
	}
}

// --- Reconcile entry-point tests ---

func TestReconcile_CRNotFound(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(testScheme).Build()
	r := newTestReconciler(cli, &fakeManifestProvider{}, testOperandImage)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestReconcile_ManifestProviderError(t *testing.T) {
	cr := newTestCR()
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	provider := &fakeManifestProvider{err: fmt.Errorf("render failed")}
	r := newTestReconciler(cli, provider, testOperandImage)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error from manifest provider, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition, found none")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %v, want False", c.Status)
	}
	if c.Reason != "ManifestRenderFailed" {
		t.Errorf("condition reason = %q, want %q", c.Reason, "ManifestRenderFailed")
	}
}

func TestReconcile_EmptyOperandImage_PassesEmptyToProvider(t *testing.T) {
	cr := newTestCR()

	var capturedParams manifests.Params
	provider := &capturingManifestProvider{
		delegate: &fakeManifestProvider{err: fmt.Errorf("stop after capture")},
		capture:  func(p manifests.Params) { capturedParams = p },
	}

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, provider, "")

	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})

	if capturedParams.OperandImage != "" {
		t.Errorf("OperandImage = %q, want empty string", capturedParams.OperandImage)
	}
	if capturedParams.OperandNamespace != testPodNamespace {
		t.Errorf("OperandNamespace = %q, want %q", capturedParams.OperandNamespace, testPodNamespace)
	}
}

// --- checkDeploymentsReady tests ---

func TestCheckDeploymentsReady_AllAvailable(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(dep).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("controller-manager", "target-ns"),
	}

	result, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if !ready {
		t.Fatal("expected ready=true")
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestCheckDeploymentsReady_NotEnoughReplicas(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(2)},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 0},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(dep).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("controller-manager", "target-ns"),
	}

	result, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if ready {
		t.Fatal("expected ready=false")
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Reason != "DeploymentNotReady" {
		t.Errorf("condition reason = %q, want %q", c.Reason, "DeploymentNotReady")
	}
}

func TestCheckDeploymentsReady_DeploymentNotFound(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("missing-deployment", "target-ns"),
	}

	result, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if ready {
		t.Fatal("expected ready=false")
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Reason != "DeploymentNotFound" {
		t.Errorf("condition reason = %q, want %q", c.Reason, "DeploymentNotFound")
	}
}

func TestCheckDeploymentsReady_ReplicaFailureCondition(t *testing.T) {
	failMsg := "quota exceeded: requested 4 CPU, limit is 2"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Message: "generic available msg"},
				{Type: appsv1.DeploymentReplicaFailure, Message: failMsg},
			},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(dep).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("controller-manager", "target-ns"),
	}

	_, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if ready {
		t.Fatal("expected ready=false")
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Message != failMsg {
		t.Errorf("condition message = %q, want ReplicaFailure message %q", c.Message, failMsg)
	}
}

func TestCheckDeploymentsReady_MultipleDeployments(t *testing.T) {
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	notReady := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 0},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(ready, notReady).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("manager", "target-ns"),
		newDeploymentUnstructured("webhook", "target-ns"),
	}

	result, isReady := r.checkDeploymentsReady(context.Background(), desired, cm)
	if isReady {
		t.Fatal("expected ready=false when one deployment is not available")
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Reason != "DeploymentNotReady" {
		t.Errorf("condition reason = %q, want %q", c.Reason, "DeploymentNotReady")
	}
}

// --- Full reconcile with condition aggregation ---

func TestReconcile_ConditionAggregation_OnManifestError(t *testing.T) {
	cr := newTestCR()
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{err: fmt.Errorf("render failed")}, testOperandImage)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	tests := []struct {
		condType string
		status   metav1.ConditionStatus
	}{
		{v1alpha1.ConditionMCPLifecycleOperatorAvailable, metav1.ConditionFalse},
		{string(platformcommon.ConditionTypeReady), metav1.ConditionFalse},
		{string(platformcommon.ConditionTypeProvisioningSucceeded), metav1.ConditionFalse},
		{string(platformcommon.ConditionTypeDegraded), metav1.ConditionFalse},
	}
	for _, tt := range tests {
		c := findCondition(updated, tt.condType)
		if c == nil {
			t.Errorf("expected condition %q, found none", tt.condType)
			continue
		}
		if c.Status != tt.status {
			t.Errorf("condition %q status = %v, want %v", tt.condType, c.Status, tt.status)
		}
	}

	if updated.Status.Phase != platformcommon.PhaseNotReady {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, platformcommon.PhaseNotReady)
	}
	if updated.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", updated.Status.ObservedGeneration)
	}
}

func TestCheckDeploymentsReady_NilReplicas(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(dep).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("controller-manager", "target-ns"),
	}

	_, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if !ready {
		t.Fatal("expected ready=true when Replicas is nil (defaults to 1) and AvailableReplicas=1")
	}
}

func TestReconcile_StatusPatch_SetsReleaseInfo(t *testing.T) {
	cr := newTestCR()
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			distributionNameKey:    "SelfManagedRHOAI",
			distributionVersionKey: "2.20.0",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, platformCM).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{err: fmt.Errorf("render failed")}, testOperandImage)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error for manifest render failure, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	releases := updated.Status.ComponentReleaseStatus.Releases
	if len(releases) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(releases))
	}

	releasesByName := make(map[string]platformcommon.ComponentRelease, len(releases))
	for _, r := range releases {
		releasesByName[r.Name] = r
	}

	moduleRelease, ok := releasesByName[v1alpha1.MCPLifecycleOperatorServiceName]
	if !ok {
		t.Fatal("missing module release entry")
	}
	if moduleRelease.Version != testOperatorVersion {
		t.Errorf("module release version = %q, want %q", moduleRelease.Version, testOperatorVersion)
	}

	platformRelease, ok := releasesByName[platformReleaseName]
	if !ok {
		t.Fatal("missing platform release entry")
	}
	if platformRelease.Version != "2.20.0" {
		t.Errorf("platform release version = %q, want %q", platformRelease.Version, "2.20.0")
	}
}

func TestReconcile_StatusPatch_NoPlatformConfigMap(t *testing.T) {
	cr := newTestCR()
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{err: fmt.Errorf("render failed")}, testOperandImage)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error for manifest render failure, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	releases := updated.Status.ComponentReleaseStatus.Releases
	if len(releases) != 2 {
		t.Fatalf("expected 2 releases (module + platform with operator version), got %d", len(releases))
	}
}

func TestReconcile_Distribution_NotSetOnFailure(t *testing.T) {
	cr := newTestCR()
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			distributionNameKey:    "OpenDataHub",
			distributionVersionKey: "3.5.1",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, platformCM).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{err: fmt.Errorf("render failed")}, testOperandImage)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected reconcile error, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	if updated.Status.Distribution.Name != "" {
		t.Errorf("distribution name = %q, want empty (not set on failed reconcile)", updated.Status.Distribution.Name)
	}
	if updated.Status.Distribution.Version != "" {
		t.Errorf("distribution version = %q, want empty (not set on failed reconcile)", updated.Status.Distribution.Version)
	}
}

func TestReconcile_Distribution_SetOnSuccess(t *testing.T) {
	cr := newTestCR()
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			distributionNameKey:    "SelfManagedRHOAI",
			distributionVersionKey: "3.5.1",
		},
	}

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr, platformCM)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	if updated.Status.Distribution.Name != "SelfManagedRHOAI" {
		t.Errorf("distribution name = %q, want %q", updated.Status.Distribution.Name, "SelfManagedRHOAI")
	}
	if updated.Status.Distribution.Version != "3.5.1" {
		t.Errorf("distribution version = %q, want %q", updated.Status.Distribution.Version, "3.5.1")
	}
}

func TestReconcile_Distribution_Standalone(t *testing.T) {
	cr := newTestCR()

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	if updated.Status.Distribution.Name != "Standalone" {
		t.Errorf("distribution name = %q, want %q", updated.Status.Distribution.Name, "Standalone")
	}
	if updated.Status.Distribution.Version != testOperatorVersion {
		t.Errorf("distribution version = %q, want %q", updated.Status.Distribution.Version, testOperatorVersion)
	}
}

func TestReconcile_Distribution_FallbackToPlatformVersionKey(t *testing.T) {
	cr := newTestCR()
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			platformVersionKey: "2.20.0",
		},
	}

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr, platformCM)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	if updated.Status.Distribution.Name != "Standalone" {
		t.Errorf("distribution name = %q, want %q (no distribution.name key)", updated.Status.Distribution.Name, "Standalone")
	}
	if updated.Status.Distribution.Version != "2.20.0" {
		t.Errorf("distribution version = %q, want %q (fallback to platformVersion)", updated.Status.Distribution.Version, "2.20.0")
	}
}

func TestCheckDeploymentsReady_AvailableConditionFallback(t *testing.T) {
	availMsg := "Deployment has minimum availability"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: "target-ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(2)},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Message: availMsg},
			},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(dep).
		Build()

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(cli, nil, testOperandImage)

	desired := []unstructured.Unstructured{
		newDeploymentUnstructured("controller-manager", "target-ns"),
	}

	_, ready := r.checkDeploymentsReady(context.Background(), desired, cm)
	if ready {
		t.Fatal("expected ready=false")
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Message != availMsg {
		t.Errorf("condition message = %q, want Available condition message %q", c.Message, availMsg)
	}
}

func TestReconcile_RequeueDelay(t *testing.T) {
	if defaultRequeueDelay != 10*time.Second {
		t.Errorf("defaultRequeueDelay = %v, want 10s", defaultRequeueDelay)
	}
}

func TestReconcile_PlatformVersionUpdate(t *testing.T) {
	cr := newTestCR()
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			platformVersionKey: "2.20.0",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, platformCM).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{err: fmt.Errorf("stop early")}, testOperandImage)

	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})

	updated := &v1alpha1.MCPLifecycleOperator{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}

	releasesByName := make(map[string]platformcommon.ComponentRelease)
	for _, r := range updated.Status.ComponentReleaseStatus.Releases {
		releasesByName[r.Name] = r
	}
	if v := releasesByName[platformReleaseName].Version; v != "2.20.0" {
		t.Fatalf("initial platform version = %q, want %q", v, "2.20.0")
	}

	platformCM.Data[platformVersionKey] = "2.21.0"
	if err := cli.Update(context.Background(), platformCM); err != nil {
		t.Fatalf("failed to update ConfigMap: %v", err)
	}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})

	if err := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); err != nil {
		t.Fatalf("failed to get updated CR after ConfigMap change: %v", err)
	}

	releasesByName = make(map[string]platformcommon.ComponentRelease)
	for _, r := range updated.Status.ComponentReleaseStatus.Releases {
		releasesByName[r.Name] = r
	}
	if v := releasesByName[platformReleaseName].Version; v != "2.21.0" {
		t.Errorf("platform version after ConfigMap update = %q, want %q", v, "2.21.0")
	}
}

func TestPlatformConfigPredicate(t *testing.T) {
	tests := []struct {
		name    string
		cmName  string
		matched bool
	}{
		{
			name:    "matches platform config ConfigMap",
			cmName:  platformConfigName,
			matched: true,
		},
		{
			name:    "ignores unrelated ConfigMap",
			cmName:  "some-other-configmap",
			matched: false,
		},
	}

	pred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == platformConfigName
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.cmName,
					Namespace: testPodNamespace,
				},
			}
			if got := pred.Generic(event.GenericEvent{Object: cm}); got != tt.matched {
				t.Errorf("predicate for %q = %v, want %v", tt.cmName, got, tt.matched)
			}
		})
	}
}
