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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	"github.com/opendatahub-io/mcp-lifecycle-module-operator/internal/manifests"
)

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(appsv1.AddToScheme(s))
	return s
}()

const (
	testPodNamespace   = "operator-ns"
	testOperatorVersion = "v0.1.0-test"
)

// --- Fake manifest provider ---

type fakeManifestProvider struct {
	resources []unstructured.Unstructured
	err       error
}

func (f *fakeManifestProvider) Manifests(_ context.Context, _ manifests.Params) ([]unstructured.Unstructured, error) {
	return f.resources, f.err
}

// --- Test helpers ---

func newTestReconciler(cli client.Client, provider manifests.Provider) *MCPLifecycleOperatorReconciler {
	return &MCPLifecycleOperatorReconciler{
		Client:           cli,
		Scheme:           testScheme,
		ManifestProvider: provider,
		OperatorVersion:  testOperatorVersion,
		PodNamespace:     testPodNamespace,
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

func newPlatformConfigMap(image, operandNS string) *corev1.ConfigMap {
	data := map[string]string{}
	if image != "" {
		data["operand-image"] = image
	}
	if operandNS != "" {
		data["operand-namespace"] = operandNS
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigMapName,
			Namespace: testPodNamespace,
		},
		Data: data,
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

// --- findDeploymentNames tests (existing) ---

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
	r := newTestReconciler(cli, &fakeManifestProvider{})

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

func TestReconcile_ConfigMapMissing(t *testing.T) {
	cr := newTestCR()
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error for missing ConfigMap, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition, found none")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %v, want False", c.Status)
	}
	if c.Reason != "ConfigMapNotFound" {
		t.Errorf("condition reason = %q, want %q", c.Reason, "ConfigMapNotFound")
	}
}

func TestReconcile_ConfigMapEmptyImage(t *testing.T) {
	cr := newTestCR()
	cm := newPlatformConfigMap("", "some-ns")
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, cm).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error for empty operand-image, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition, found none")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %v, want False", c.Status)
	}
}

func TestReconcile_ManifestProviderError(t *testing.T) {
	cr := newTestCR()
	cm := newPlatformConfigMap("registry.io/image:v1", "target-ns")
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, cm).
		WithStatusSubresource(cr).
		Build()

	provider := &fakeManifestProvider{err: fmt.Errorf("render failed")}
	r := newTestReconciler(cli, provider)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error from manifest provider, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
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

// --- readPlatformConfig tests ---

func TestReadPlatformConfig_NamespaceDefault(t *testing.T) {
	cm := newPlatformConfigMap("registry.io/image:v1", "")
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cm).
		Build()

	r := newTestReconciler(cli, nil)

	image, ns, err := r.readPlatformConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if image != "registry.io/image:v1" {
		t.Errorf("image = %q, want %q", image, "registry.io/image:v1")
	}
	if ns != "mcp-lifecycle-operator-system" {
		t.Errorf("namespace = %q, want %q", ns, "mcp-lifecycle-operator-system")
	}
}

func TestReadPlatformConfig_CustomNamespace(t *testing.T) {
	cm := newPlatformConfigMap("registry.io/image:v1", "custom-ns")
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cm).
		Build()

	r := newTestReconciler(cli, nil)

	image, ns, err := r.readPlatformConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if image != "registry.io/image:v1" {
		t.Errorf("image = %q, want %q", image, "registry.io/image:v1")
	}
	if ns != "custom-ns" {
		t.Errorf("namespace = %q, want %q", ns, "custom-ns")
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
	r := newTestReconciler(cli, nil)

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
	r := newTestReconciler(cli, nil)

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
	r := newTestReconciler(cli, nil)

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
	r := newTestReconciler(cli, nil)

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
	r := newTestReconciler(cli, nil)

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

func TestReconcile_ConditionAggregation_OnConfigError(t *testing.T) {
	cr := newTestCR()
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
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
	r := newTestReconciler(cli, nil)

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
	cm := newPlatformConfigMap("", "ns")
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(cr, cm).
		WithStatusSubresource(cr).
		Build()

	r := newTestReconciler(cli, &fakeManifestProvider{})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err == nil {
		t.Fatal("expected error for empty operand-image config, got nil")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := cli.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	releases := updated.Status.ComponentReleaseStatus.Releases
	if len(releases) != 1 {
		t.Fatalf("expected 1 release, got %d", len(releases))
	}
	if releases[0].Version != testOperatorVersion {
		t.Errorf("release version = %q, want %q", releases[0].Version, testOperatorVersion)
	}
	if releases[0].Name != v1alpha1.MCPLifecycleOperatorServiceName {
		t.Errorf("release name = %q, want %q", releases[0].Name, v1alpha1.MCPLifecycleOperatorServiceName)
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
	r := newTestReconciler(cli, nil)

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
