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

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var gatewayListKinds = map[schema.GroupVersionResource]string{
	mcpGatewayExtensionGVR: "MCPGatewayExtensionList",
	gatewayGVR:             "GatewayList",
}

func newMCPGatewayExtension(namespace, name, gwName, gwNamespace, sectionName string, ready bool) *unstructured.Unstructured {
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}

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
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": readyStatus,
				},
			},
		},
	}}
}

func newGateway(namespace, name string, listeners ...string) *unstructured.Unstructured {
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

// newFakeDynamicForGateways builds a fake dynamic client pre-seeded with the
// given MCPGatewayExtension and Gateway unstructured objects. Gateway objects
// must be added via Create because meta.UnsafeGuessKindToResource pluralises
// "Gateway" as "gatewaies", so tracker.Add stores them under the wrong GVR.
func newFakeDynamicForGateways(objects ...*unstructured.Unstructured) *fakedynamic.FakeDynamicClient {
	allKinds := map[schema.GroupVersionResource]string{}
	for k, v := range mcpServerListKinds {
		allKinds[k] = v
	}
	for k, v := range gatewayListKinds {
		allKinds[k] = v
	}

	client := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(testScheme, allKinds)

	for _, obj := range objects {
		gvk := obj.GroupVersionKind()
		var gvr schema.GroupVersionResource
		switch gvk.Kind {
		case "MCPGatewayExtension":
			gvr = mcpGatewayExtensionGVR
		case "Gateway":
			gvr = gatewayGVR
		default:
			continue
		}
		ns := obj.GetNamespace()
		if ns != "" {
			client.Resource(gvr).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}) //nolint:errcheck
		} else {
			client.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{}) //nolint:errcheck
		}
	}

	return client
}

func mcpGatewayExtensionCRD() *extv1.CustomResourceDefinition {
	return &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: mcpGatewayExtensionCRDName},
	}
}

func TestDiscoverMCPGateways_CRDNotInstalled(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(testScheme).Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Errorf("expected no error when CRD not installed, got %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result when CRD not installed, got %d entries", len(result))
	}
}

func TestDiscoverMCPGateways_NoInstances(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Errorf("expected no error when no instances exist, got %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result when no instances exist, got %d entries", len(result))
	}
}

func TestDiscoverMCPGateways_SingleExtensionWithGateway(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "my-gw", "gw-system", "mcp", true)

	gw := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": "gw-system", "name": "my-gw"},
		"spec": map[string]interface{}{
			"gatewayClassName": "istio",
			"listeners": []interface{}{
				map[string]interface{}{
					"name":     "mcp",
					"hostname": "*.mcp.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
				map[string]interface{}{
					"name":     "mcps",
					"hostname": "*.mcps.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
				map[string]interface{}{
					"name":     "direct-http",
					"hostname": "mcp.example.com",
					"port":     int64(80),
					"protocol": "HTTP",
				},
				map[string]interface{}{
					"name":     "admin",
					"hostname": "admin.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
			},
		},
	}}

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge, gw),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}

	entry := result[0]
	if entry.Name != "my-ext" || entry.Namespace != "ns1" {
		t.Errorf("unexpected MCPGatewayExtension identity: %s/%s", entry.Namespace, entry.Name)
	}
	if !entry.Ready {
		t.Error("expected Ready=true")
	}
	if entry.Gateway.Name != "my-gw" || entry.Gateway.Namespace != "gw-system" {
		t.Errorf("unexpected Gateway identity: %s/%s", entry.Gateway.Namespace, entry.Gateway.Name)
	}
	if len(entry.Gateway.Listeners) != 3 {
		t.Fatalf("expected 3 listeners (2 wildcard + 1 direct HTTP), got %d", len(entry.Gateway.Listeners))
	}
	if entry.Gateway.Listeners[0].Name != "mcp" || entry.Gateway.Listeners[0].Hostname != "*.mcp.example.com" {
		t.Errorf("unexpected listener[0]: %+v", entry.Gateway.Listeners[0])
	}
	if entry.Gateway.Listeners[1].Name != "mcps" || entry.Gateway.Listeners[1].Hostname != "*.mcps.example.com" {
		t.Errorf("unexpected listener[1]: %+v", entry.Gateway.Listeners[1])
	}
	if entry.Gateway.Listeners[2].Name != "direct-http" || entry.Gateway.Listeners[2].Hostname != "mcp.example.com" {
		t.Errorf("unexpected listener[2]: %+v", entry.Gateway.Listeners[2])
	}
}

func TestDiscoverMCPGateways_ExtensionNotReady(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "my-gw", "gw-system", "mcp", false)
	gw := newGateway("gw-system", "my-gw", "mcp")

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge, gw),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[0].Ready {
		t.Error("expected Ready=false")
	}
}

func TestDiscoverMCPGateways_GatewayNotFound(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "missing-gw", "gw-system", "mcp", true)

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if len(result[0].Gateway.Listeners) != 0 {
		t.Errorf("expected empty listeners when Gateway not found, got %d", len(result[0].Gateway.Listeners))
	}
}

func TestDiscoverMCPGateways_MultipleExtensions(t *testing.T) {
	mcpge1 := newMCPGatewayExtension("ns1", "ext1", "gw1", "gw-system", "mcp", true)
	mcpge2 := newMCPGatewayExtension("ns2", "ext2", "gw2", "gw-system", "mcps", false)
	gw1 := newGateway("gw-system", "gw1", "mcp")
	gw2 := newGateway("gw-system", "gw2", "mcps", "other")

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge1, mcpge2, gw1, gw2),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result))
	}
}

func TestDiscoverMCPGateways_DefaultNamespace(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "my-gw", "", "mcp", true)
	gw := newGateway("ns1", "my-gw", "mcp")

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge, gw),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[0].Gateway.Namespace != "ns1" {
		t.Errorf("expected Gateway namespace defaulted to ns1, got %s", result[0].Gateway.Namespace)
	}
}

func TestDiscoverMCPGateways_ListenerFiltering(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "my-gw", "ns1", "mcp", true)

	gw := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": "ns1", "name": "my-gw"},
		"spec": map[string]interface{}{
			"gatewayClassName": "istio",
			"listeners": []interface{}{
				map[string]interface{}{
					"name":     "wildcard-https",
					"hostname": "*.mcp.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
				map[string]interface{}{
					"name":     "direct-http",
					"hostname": "mcp.example.com",
					"port":     int64(80),
					"protocol": "HTTP",
				},
				map[string]interface{}{
					"name":     "direct-https",
					"hostname": "api.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
				map[string]interface{}{
					"name":     "no-hostname",
					"port":     int64(80),
					"protocol": "HTTP",
				},
			},
		},
	}}

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge, gw),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if len(result[0].Gateway.Listeners) != 2 {
		t.Fatalf("expected 2 listeners (wildcard HTTPS + direct HTTP), got %d", len(result[0].Gateway.Listeners))
	}
	if result[0].Gateway.Listeners[0].Name != "wildcard-https" || result[0].Gateway.Listeners[0].Hostname != "*.mcp.example.com" {
		t.Errorf("unexpected listener[0]: %+v", result[0].Gateway.Listeners[0])
	}
	if result[0].Gateway.Listeners[1].Name != "direct-http" || result[0].Gateway.Listeners[1].Hostname != "mcp.example.com" {
		t.Errorf("unexpected listener[1]: %+v", result[0].Gateway.Listeners[1])
	}
}

func TestDiscoverMCPGateways_NoUsableListeners(t *testing.T) {
	mcpge := newMCPGatewayExtension("ns1", "my-ext", "my-gw", "ns1", "mcp", true)

	gw := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": "ns1", "name": "my-gw"},
		"spec": map[string]interface{}{
			"gatewayClassName": "istio",
			"listeners": []interface{}{
				map[string]interface{}{
					"name":     "direct-https",
					"hostname": "api.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
				map[string]interface{}{
					"name":     "catchall",
					"port":     int64(80),
					"protocol": "HTTP",
				},
			},
		},
	}}

	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(mcpGatewayExtensionCRD()).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(mcpge, gw),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if len(result[0].Gateway.Listeners) != 0 {
		t.Errorf("expected empty listeners, got %d", len(result[0].Gateway.Listeners))
	}
}

func TestDiscoverMCPGateways_TransientError(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return fmt.Errorf("transient API error")
			},
		}).
		Build()

	r := &MCPLifecycleOperatorReconciler{
		Client:        cli,
		DynamicClient: newFakeDynamicForGateways(),
	}

	result, err := r.discoverMCPGateways(context.Background())
	if err == nil {
		t.Error("expected error on transient API failure")
	}
	if result != nil {
		t.Errorf("expected nil result on transient error, got %d entries", len(result))
	}
}

func TestIsConditionTrue(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue},
		{Type: "Accepted", Status: metav1.ConditionFalse},
	}

	if !isConditionTrue(conditions, "Ready") {
		t.Error("expected Ready to be true")
	}
	if isConditionTrue(conditions, "Accepted") {
		t.Error("expected Accepted to be false")
	}
	if isConditionTrue(conditions, "Missing") {
		t.Error("expected missing condition to be false")
	}
}
