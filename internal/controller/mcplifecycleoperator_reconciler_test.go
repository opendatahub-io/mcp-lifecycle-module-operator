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
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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
	utilruntime.Must(extv1.AddToScheme(s))
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
	return newTestReconcilerFullIntercepted(provider, operandImage, nil, objects...)
}

// newTestReconcilerFullIntercepted builds the full harness with an additional
// client Get interceptor layered on top of the standard harness interceptors,
// so tests can inject read failures (e.g. transient APIServer errors) for
// specific object types while everything else reads through normally.
func newTestReconcilerFullIntercepted(
	provider manifests.Provider,
	operandImage string,
	getInterceptor func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error,
	objects ...client.Object,
) *MCPLifecycleOperatorReconciler {
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
			Get: getInterceptor,
		}).
		Build()

	kubeClient := kubefake.NewSimpleClientset()

	return &MCPLifecycleOperatorReconciler{
		Client:           cli,
		Scheme:           testScheme,
		Deployer:         deploy.NewDeployer(),
		DynamicClient:    newFakeDynamicWithMCPServers(),
		DiscoveryClient:  kubeClient.Discovery(),
		ManifestProvider: provider,
		OperatorVersion:  testOperatorVersion,
		PodNamespace:     testPodNamespace,
		OperandImage:     operandImage,
	}
}

// mcpServerListKinds maps the MCPServer GVR to a list kind so the fake dynamic
// client can serve LIST requests for a resource that is not registered in the
// test scheme (mcp.x-k8s.io is intentionally absent, mirroring the manager
// scheme in cmd/main.go).
var mcpServerListKinds = map[schema.GroupVersionResource]string{
	mcpServerGVR: "MCPServerList",
}

// newFakeDynamicWithMCPServers builds a fake dynamic client that can LIST
// MCPServers at v1beta1, seeded with the given objects. An empty seed yields a
// successful empty list (the vacuous-pass case).
func newFakeDynamicWithMCPServers(objects ...runtime.Object) *fakedynamic.FakeDynamicClient {
	return fakedynamic.NewSimpleDynamicClientWithCustomListKinds(testScheme, mcpServerListKinds, objects...)
}

// recordingDynamic wraps a dynamic.Interface and records the Continue token of
// every List call. The fake dynamic client's recorded action drops
// ListOptions.Continue (NewRootListActionWithOptions keeps only the label/field
// selectors), so a PrependReactor cannot observe it; wrapping the client at the
// List boundary is the only way to assert pagination-token handling.
type recordingDynamic struct {
	dynamic.Interface
	listContinues *[]string
}

func (r *recordingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingResource{
		NamespaceableResourceInterface: r.Interface.Resource(gvr),
		listContinues:                  r.listContinues,
	}
}

type recordingResource struct {
	dynamic.NamespaceableResourceInterface
	listContinues *[]string
}

func (r *recordingResource) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	*r.listContinues = append(*r.listContinues, opts.Continue)
	return r.NamespaceableResourceInterface.List(ctx, opts)
}

// newMCPServer returns an unstructured MCPServer at v1beta1 for seeding the
// fake dynamic client.
func newMCPServer(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "mcp.x-k8s.io/v1beta1",
		"kind":       "MCPServer",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
	}}
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

// TestReconcile_StatusPatch_ReleasesPlatformHeldBackOnFailure asserts that a
// failed reconcile still records the module's own release but does NOT advance
// status.releases.platform - the field the platform operator reads to track
// upgrade completion. The platform release is derived from status.distribution
// (the gated, committed value), which setDistributionStatus leaves untouched on
// a failed reconcile, so it advances only in lockstep with the conversion-health
// handshake.
func TestReconcile_StatusPatch_ReleasesPlatformHeldBackOnFailure(t *testing.T) {
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
	if len(releases) != 1 {
		t.Fatalf("expected 1 release (module only, platform held back), got %d: %+v", len(releases), releases)
	}

	releasesByName := make(map[string]platformcommon.ComponentRelease, len(releases))
	for _, rel := range releases {
		releasesByName[rel.Name] = rel
	}

	moduleRelease, ok := releasesByName[v1alpha1.MCPLifecycleOperatorServiceName]
	if !ok {
		t.Fatal("missing module release entry")
	}
	if moduleRelease.Version != testOperatorVersion {
		t.Errorf("module release version = %q, want %q", moduleRelease.Version, testOperatorVersion)
	}

	if _, ok := releasesByName[platformReleaseName]; ok {
		t.Errorf("platform release entry present on failed reconcile, want it held back until the handshake commits status.distribution")
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

	// The platform release is gated on status.distribution, which a failed
	// reconcile never advances, so only the module's own release is recorded.
	releases := updated.Status.ComponentReleaseStatus.Releases
	if len(releases) != 1 {
		t.Fatalf("expected 1 release (module only, platform held back), got %d: %+v", len(releases), releases)
	}
}

// TestReconcile_StatusPatch_ReleasesPlatformSetOnSuccess asserts the positive
// side of the gate: after a fully successful reconcile the committed platform
// version is published to status.releases.platform, matching status.distribution.
func TestReconcile_StatusPatch_ReleasesPlatformSetOnSuccess(t *testing.T) {
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

	releases := updated.Status.ComponentReleaseStatus.Releases
	releasesByName := make(map[string]platformcommon.ComponentRelease, len(releases))
	for _, rel := range releases {
		releasesByName[rel.Name] = rel
	}

	platformRelease, ok := releasesByName[platformReleaseName]
	if !ok {
		t.Fatalf("missing platform release entry after successful reconcile, got %+v", releases)
	}
	if platformRelease.Version != "3.5.1" {
		t.Errorf("platform release version = %q, want %q", platformRelease.Version, "3.5.1")
	}
	if platformRelease.Version != updated.Status.Distribution.Version {
		t.Errorf("platform release version %q != committed distribution version %q (must advance in lockstep)",
			platformRelease.Version, updated.Status.Distribution.Version)
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

// TestReconcile_PlatformVersionUpdate exercises the upgrade window: when the
// platform ConfigMap advances the desired version, a successful reconcile
// advances status.releases.platform in lockstep with the committed
// status.distribution. Both reconciles run the full success path (the
// conversion-health gate passes over an empty MCPServer list) so the platform
// release moves only once the handshake has committed - the field the platform
// operator reads to track upgrade completion.
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

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr, platformCM)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error on initial reconcile: %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}

	releasesByName := make(map[string]platformcommon.ComponentRelease)
	for _, rel := range updated.Status.ComponentReleaseStatus.Releases {
		releasesByName[rel.Name] = rel
	}
	if v := releasesByName[platformReleaseName].Version; v != "2.20.0" {
		t.Fatalf("initial platform version = %q, want %q", v, "2.20.0")
	}

	platformCM.Data[platformVersionKey] = "2.21.0"
	if err := r.Update(context.Background(), platformCM); err != nil {
		t.Fatalf("failed to update ConfigMap: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error on reconcile after ConfigMap change: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); err != nil {
		t.Fatalf("failed to get updated CR after ConfigMap change: %v", err)
	}

	releasesByName = make(map[string]platformcommon.ComponentRelease)
	for _, rel := range updated.Status.ComponentReleaseStatus.Releases {
		releasesByName[rel.Name] = rel
	}
	if v := releasesByName[platformReleaseName].Version; v != "2.21.0" {
		t.Errorf("platform version after ConfigMap update = %q, want %q", v, "2.21.0")
	}
}

// --- checkConversionHealth tests ---

func newConversionTestReconciler(dyn *fakedynamic.FakeDynamicClient) (*MCPLifecycleOperatorReconciler, *v1alpha1.MCPLifecycleOperator, *v1alpha1.ConditionsManager) {
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newTestReconciler(nil, nil, testOperandImage)
	r.DynamicClient = dyn
	return r, cr, cm
}

func TestCheckConversionHealth_Healthy(t *testing.T) {
	dyn := newFakeDynamicWithMCPServers(newMCPServer("ns-a", "server-1"))
	r, _, cm := newConversionTestReconciler(dyn)

	result, ready := r.checkConversionHealth(context.Background(), cm)
	if !ready {
		t.Fatal("expected ready=true when MCPServers list successfully")
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestCheckConversionHealth_NoObjects_VacuousPass(t *testing.T) {
	dyn := newFakeDynamicWithMCPServers()
	r, cr, cm := newConversionTestReconciler(dyn)

	result, ready := r.checkConversionHealth(context.Background(), cm)
	if !ready {
		t.Fatal("expected ready=true for an empty MCPServer list (vacuous pass)")
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %v", result)
	}
	if c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable); c != nil && c.Status == metav1.ConditionFalse {
		t.Errorf("gate must not mark Available false on a vacuous pass, got reason %q", c.Reason)
	}
}

func TestCheckConversionHealth_ListError_Requeues(t *testing.T) {
	dyn := newFakeDynamicWithMCPServers()
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("conversion webhook for mcpservers.mcp.x-k8s.io failed: x509: certificate signed by unknown authority")
	})
	r, cr, cm := newConversionTestReconciler(dyn)

	result, ready := r.checkConversionHealth(context.Background(), cm)
	if ready {
		t.Fatal("expected ready=false when the MCPServer LIST errors")
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Reason != reasonConversionCheckFailed {
		t.Errorf("condition reason = %q, want %q", c.Reason, reasonConversionCheckFailed)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %v, want False", c.Status)
	}
	if !strings.Contains(c.Message, "x509") {
		t.Errorf("condition message = %q, want it to carry the underlying error", c.Message)
	}
}

func TestCheckConversionHealth_Pending_WhenAPINotServed(t *testing.T) {
	// The dynamic client issues raw REST calls, so an unserved MCPServer v1beta1
	// resource comes back as a 404 NotFound - a LIST of a served resource with no
	// objects would instead return an empty list. The scheme/mapper variants are
	// handled defensively too. All must defer via ConversionCheckPending rather
	// than the failure path.
	cases := map[string]error{
		"NotFound":        k8serr.NewNotFound(mcpServerGVR.GroupResource(), ""),
		"NoResourceMatch": &meta.NoResourceMatchError{PartialResource: mcpServerGVR},
		"NotRegistered":   runtime.NewNotRegisteredErrForKind("test-scheme", mcpServerGVR.GroupVersion().WithKind("MCPServer")),
	}
	for name, injErr := range cases {
		t.Run(name, func(t *testing.T) {
			dyn := newFakeDynamicWithMCPServers()
			dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, injErr
			})
			r, cr, cm := newConversionTestReconciler(dyn)

			result, ready := r.checkConversionHealth(context.Background(), cm)
			if ready {
				t.Fatal("expected ready=false when the MCPServer resource is not served")
			}
			if result.RequeueAfter != defaultRequeueDelay {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
			}

			c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
			if c == nil {
				t.Fatal("expected MCPLifecycleOperatorAvailable condition")
			}
			if c.Reason != reasonConversionCheckPending {
				t.Errorf("condition reason = %q, want %q", c.Reason, reasonConversionCheckPending)
			}
		})
	}
}

func TestCheckConversionHealth_ExpiredContinue_Restarts(t *testing.T) {
	// An expired continue token (HTTP 410 Gone) on a follow-up page is a normal
	// pagination event on a churning cluster, not a conversion failure. The gate
	// must restart listing from the first page rather than marking the operand
	// unavailable.
	dyn := newFakeDynamicWithMCPServers()
	calls := 0
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: mcpServerGVR.Group, Version: mcpServerGVR.Version, Kind: "MCPServerList",
		})

		switch calls {
		case 1:
			// First page hands out a continue token.
			list.Items = []unstructured.Unstructured{*newMCPServer("ns", "server-1")}
			list.SetContinue("page-2-token")
		case 2:
			// The second page's continue token has expired.
			return true, nil, k8serr.NewResourceExpired("continue token expired")
		case 3:
			// Restarted from the first page (no continue token); finish cleanly.
			list.Items = []unstructured.Unstructured{*newMCPServer("ns", "server-1")}
			list.SetContinue("")
		default:
			t.Fatalf("unexpected LIST call #%d (gate did not restart on expiry)", calls)
		}

		return true, list, nil
	})
	r, cr, cm := newConversionTestReconciler(dyn)

	// The fake client's recorded action discards ListOptions.Continue, so wrap
	// the client to capture the token actually sent on each List. This asserts
	// the token is reset to "" on restart (production line ~510) rather than
	// only that a third call happened - a regression dropping that reset would
	// otherwise pass on call count alone.
	var continues []string
	r.DynamicClient = &recordingDynamic{Interface: dyn, listContinues: &continues}

	result, ready := r.checkConversionHealth(context.Background(), cm)
	if !ready {
		t.Fatal("expected ready=true after restarting a paginated list on an expired token")
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %v", result)
	}
	if calls != 3 {
		t.Errorf("expected 3 LIST calls (restart after expiry), got %d", calls)
	}
	wantContinues := []string{"", "page-2-token", ""}
	if len(continues) != len(wantContinues) {
		t.Fatalf("recorded %d List calls, want %d: %v", len(continues), len(wantContinues), continues)
	}
	for i, want := range wantContinues {
		if continues[i] != want {
			t.Errorf("List call %d Continue = %q, want %q (token must reset to empty on restart)", i+1, continues[i], want)
		}
	}
	if c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable); c != nil && c.Status == metav1.ConditionFalse {
		t.Errorf("expired token must not mark Available false, got reason %q", c.Reason)
	}
}

func TestCheckConversionHealth_ExpiredContinue_GivesUpAfterCap(t *testing.T) {
	// If the continue token keeps expiring, the gate must not loop forever: it
	// restarts a bounded number of times, then requeues (pending) so
	// controller-runtime re-drives with backoff rather than spinning the worker.
	dyn := newFakeDynamicWithMCPServers()
	calls := 0
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, nil, k8serr.NewResourceExpired("continue token expired")
	})
	r, cr, cm := newConversionTestReconciler(dyn)

	result, ready := r.checkConversionHealth(context.Background(), cm)
	if ready {
		t.Fatal("expected ready=false when pagination keeps expiring")
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRequeueDelay)
	}
	if want := conversionCheckMaxRestarts + 1; calls != want {
		t.Errorf("expected %d LIST calls (bounded restarts, then give up), got %d", want, calls)
	}

	c := findCondition(cr, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Reason != reasonConversionCheckPending {
		t.Errorf("condition reason = %q, want %q", c.Reason, reasonConversionCheckPending)
	}
}

func TestCheckConversionHealth_Paginated(t *testing.T) {
	// The fake dynamic client does not honor Limit/Continue on its own, so drive
	// pagination explicitly: the first LIST returns a continue token, the second
	// returns none. Asserting exactly two calls proves the gate follows Continue
	// rather than stopping after the first page.
	dyn := newFakeDynamicWithMCPServers()
	calls := 0
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: mcpServerGVR.Group, Version: mcpServerGVR.Version, Kind: "MCPServerList",
		})

		switch calls {
		case 1:
			// First page returns a continue token; the gate must issue a second
			// LIST. A gate that ignored GetContinue would stop here (calls == 1).
			list.Items = []unstructured.Unstructured{*newMCPServer("ns", "server-1")}
			list.SetContinue("page-2-token")
		case 2:
			// Empty continue token terminates the loop.
			list.Items = []unstructured.Unstructured{*newMCPServer("ns", "server-2")}
			list.SetContinue("")
		default:
			t.Fatalf("unexpected LIST call #%d (gate did not stop on empty continue)", calls)
		}

		return true, list, nil
	})
	r, _, cm := newConversionTestReconciler(dyn)

	_, ready := r.checkConversionHealth(context.Background(), cm)
	if !ready {
		t.Fatal("expected ready=true across a paginated MCPServer list")
	}
	if calls != 2 {
		t.Errorf("expected 2 LIST calls (Continue followed once), got %d", calls)
	}
}

// --- Full reconcile: conversion gate ---

func TestReconcile_ConversionFailing_DoesNotAdvanceDistribution(t *testing.T) {
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
	dyn := newFakeDynamicWithMCPServers()
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("conversion webhook unavailable")
	})
	r.DynamicClient = dyn

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error (gate must requeue, not error): %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}

	if updated.Status.Distribution.Name != "" || updated.Status.Distribution.Version != "" {
		t.Errorf("distribution = %+v, want empty (conversion gate blocked the version write)", updated.Status.Distribution)
	}

	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil {
		t.Fatal("expected MCPLifecycleOperatorAvailable condition")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %v, want False", c.Status)
	}
	if c.Reason != reasonConversionCheckFailed {
		t.Errorf("condition reason = %q, want %q", c.Reason, reasonConversionCheckFailed)
	}
}

func TestReconcile_ConversionRecovers_RecordsDistribution(t *testing.T) {
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
	dyn := newFakeDynamicWithMCPServers()
	failing := true
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, fmt.Errorf("conversion webhook unavailable")
		}
		return false, nil, nil // fall through to the tracker (empty list, healthy)
	})
	r.DynamicClient = dyn

	// First reconcile: conversion failing -> gate blocks, distribution unset.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error on first reconcile: %v", err)
	}

	afterFail := &v1alpha1.MCPLifecycleOperator{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, afterFail); err != nil {
		t.Fatalf("failed to get CR after first reconcile: %v", err)
	}
	if afterFail.Status.Distribution.Version != "" {
		t.Fatalf("distribution set while conversion failing: %+v", afterFail.Status.Distribution)
	}

	// Conversion recovers, no manual intervention beyond the normal requeue.
	failing = false
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); err != nil {
		t.Fatalf("failed to get CR after recovery: %v", err)
	}
	if updated.Status.Distribution.Name != "SelfManagedRHOAI" || updated.Status.Distribution.Version != "3.5.1" {
		t.Errorf("distribution = %+v, want it recorded after recovery", updated.Status.Distribution)
	}
	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Available condition = %+v, want True after recovery", c)
	}
}

func TestReconcile_SteadyState_SkipsConversionLIST(t *testing.T) {
	cr := newTestCR()
	// status.distribution already matches the platform config AND this controller
	// previously recorded that it verified the conversion: the handshake has
	// settled, so the conversion gate must not re-LIST MCPServers.
	cr.Status.Distribution = v1alpha1.Distribution{Name: "SelfManagedRHOAI", Version: "3.5.1"}
	v1alpha1.NewConditionsManager(cr, cr.Generation).
		MarkTrueWithReason(v1alpha1.ConditionMCPServerConversionVerified, "ConversionVerified")
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
	listCalls := 0
	dyn := newFakeDynamicWithMCPServers()
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return true, nil, fmt.Errorf("conversion LIST must be skipped once the handshake has settled")
	})
	r.DynamicClient = dyn

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listCalls != 0 {
		t.Errorf("expected 0 conversion LIST calls in steady state, got %d", listCalls)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}
	if updated.Status.Distribution.Version != "3.5.1" {
		t.Errorf("distribution version = %q, want %q (unchanged)", updated.Status.Distribution.Version, "3.5.1")
	}
	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Available condition = %+v, want True in steady state", c)
	}
}

// TestReconcile_TransientTLSFallback_SchedulesRetry covers the fallback
// reconcile that schedules a retry without an APIServer watch event: a
// transient APIServer TLS read must not degrade the operand (Available stays
// True, Intermediate defaults are projected) but must schedule a requeue so
// the resolved profile converges once the API server recovers.
func TestReconcile_TransientTLSFallback_SchedulesRetry(t *testing.T) {
	cr := newTestCR()
	// Mark the storage-version migration as settled so the only requeue source
	// in an otherwise fully successful reconcile is the transient-fallback
	// retry at the end of reconcile().
	v1alpha1.NewConditionsManager(cr, cr.Generation).
		MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)

	var captured manifests.Params
	provider := &capturingManifestProvider{
		delegate: &fakeManifestProvider{},
		capture:  func(p manifests.Params) { captured = p },
	}

	r := newTestReconcilerFullIntercepted(provider, testOperandImage,
		func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*configv1.APIServer); ok {
				return k8serr.NewServiceUnavailable("temporary API server failure")
			}
			return c.Get(ctx, key, obj, opts...)
		}, cr)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != defaultRequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v (transient fallback must schedule a retry)", result.RequeueAfter, defaultRequeueDelay)
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}
	c := findCondition(updated, v1alpha1.ConditionMCPLifecycleOperatorAvailable)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Available condition = %+v, want True: a transient profile read must not degrade the operand", c)
	}

	if captured.TLSMinVersion != string(configv1.VersionTLS12) || captured.TLSCipherSuites == "" {
		t.Errorf("operand TLS params = min %q ciphers %q, want Intermediate fallback values",
			captured.TLSMinVersion, captured.TLSCipherSuites)
	}
}

// TestReconcile_SteadyState_NoRequeueWithoutTransientFallback is the control
// case for the transient-fallback retry: the same settled reconcile without a
// transient APIServer read (NotFound on a non-OpenShift fake cluster) must
// return a zero result, because nothing is pending convergence.
func TestReconcile_SteadyState_NoRequeueWithoutTransientFallback(t *testing.T) {
	cr := newTestCR()
	v1alpha1.NewConditionsManager(cr, cr.Generation).
		MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 without a transient fallback", result.RequeueAfter)
	}
}

// TestReconcile_FirstUpgradeAfterRollout_RunsConversionLIST covers the rollout
// that first introduces this gate: the previous (ungated) controller can
// advance status.distribution to the desired version before this controller
// runs, so status.distribution alone would (wrongly) look settled. Because the
// MCPServerConversionVerified marker is absent, the gate must still run the
// conversion LIST once, then record the marker.
func TestReconcile_FirstUpgradeAfterRollout_RunsConversionLIST(t *testing.T) {
	cr := newTestCR()
	// Distribution already at the desired version (written by the predecessor),
	// but NO verified marker: this controller has never checked conversion.
	cr.Status.Distribution = v1alpha1.Distribution{Name: "SelfManagedRHOAI", Version: "3.5.1"}
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
	listCalls := 0
	dyn := newFakeDynamicWithMCPServers()
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return false, nil, nil // fall through to tracker: empty list, healthy
	})
	r.DynamicClient = dyn

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listCalls == 0 {
		t.Error("expected the conversion LIST to run on the first post-rollout reconcile, got 0 calls")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}
	c := findCondition(updated, v1alpha1.ConditionMCPServerConversionVerified)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("MCPServerConversionVerified = %+v, want True after the check passes", c)
	}
}

// TestReconcile_UpgradeWindow_RunsConversionLIST is the counterpart to the
// steady-state test: when the desired version moves ahead of the recorded one,
// the gate must run the conversion LIST again.
func TestReconcile_UpgradeWindow_RunsConversionLIST(t *testing.T) {
	cr := newTestCR()
	cr.Status.Distribution = v1alpha1.Distribution{Name: "SelfManagedRHOAI", Version: "3.5.0"}
	platformCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigName,
			Namespace: testPodNamespace,
		},
		Data: map[string]string{
			distributionNameKey:    "SelfManagedRHOAI",
			distributionVersionKey: "3.5.1", // desired advanced beyond recorded 3.5.0
		},
	}

	r := newTestReconcilerFull(&fakeManifestProvider{}, testOperandImage, cr, platformCM)
	listCalls := 0
	dyn := newFakeDynamicWithMCPServers()
	dyn.PrependReactor("list", "mcpservers", func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return false, nil, nil // fall through to tracker: empty list, healthy
	})
	r.DynamicClient = dyn

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listCalls == 0 {
		t.Error("expected the conversion LIST to run during the upgrade window, got 0 calls")
	}

	updated := &v1alpha1.MCPLifecycleOperator{}
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, updated); getErr != nil {
		t.Fatalf("failed to get updated CR: %v", getErr)
	}
	if updated.Status.Distribution.Version != "3.5.1" {
		t.Errorf("distribution version = %q, want %q (advanced after healthy gate)", updated.Status.Distribution.Version, "3.5.1")
	}
}

// TestConversionHandshakeSettled pins the 2x2 matrix that decides whether the
// conversion gate's cluster-wide LIST is skipped. Only the all-match case is
// settled; an unavailable config or any distribution drift (name or version)
// must re-open the gate so a stale/broken conversion webhook is re-checked on
// the next upgrade.
func TestConversionHandshakeSettled(t *testing.T) {
	const (
		name    = "SelfManagedRHOAI"
		version = "3.5.1"
	)
	tests := []struct {
		name     string
		dist     v1alpha1.Distribution
		verified bool
		pc       platformConfig
		want     bool
	}{
		{
			name:     "settled: available, distribution matches, and conversion verified",
			dist:     v1alpha1.Distribution{Name: name, Version: version},
			verified: true,
			pc:       platformConfig{Available: true, DistributionName: name, DistributionVersion: version},
			want:     true,
		},
		{
			name:     "not settled: distribution matches but conversion not yet verified (first rollout)",
			dist:     v1alpha1.Distribution{Name: name, Version: version},
			verified: false,
			pc:       platformConfig{Available: true, DistributionName: name, DistributionVersion: version},
			want:     false,
		},
		{
			name:     "not settled: config unavailable",
			dist:     v1alpha1.Distribution{Name: name, Version: version},
			verified: true,
			pc:       platformConfig{Available: false, DistributionName: name, DistributionVersion: version},
			want:     false,
		},
		{
			name:     "not settled: distribution name mismatch",
			dist:     v1alpha1.Distribution{Name: "ManagedRHOAI", Version: version},
			verified: true,
			pc:       platformConfig{Available: true, DistributionName: name, DistributionVersion: version},
			want:     false,
		},
		{
			name:     "not settled: distribution version mismatch (upgrade window)",
			dist:     v1alpha1.Distribution{Name: name, Version: "3.5.0"},
			verified: true,
			pc:       platformConfig{Available: true, DistributionName: name, DistributionVersion: version},
			want:     false,
		},
		{
			name:     "not settled: distribution unset",
			dist:     v1alpha1.Distribution{},
			verified: true,
			pc:       platformConfig{Available: true, DistributionName: name, DistributionVersion: version},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := newTestCR()
			cr.Status.Distribution = tt.dist
			if tt.verified {
				v1alpha1.NewConditionsManager(cr, cr.Generation).
					MarkTrueWithReason(v1alpha1.ConditionMCPServerConversionVerified, "ConversionVerified")
			}
			if got := conversionHandshakeSettled(cr, tt.pc); got != tt.want {
				t.Errorf("conversionHandshakeSettled() = %v, want %v", got, tt.want)
			}
		})
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
