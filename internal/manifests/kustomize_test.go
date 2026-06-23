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

package manifests

import (
	"context"
	"testing"
	"testing/fstest"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	odhLabels "github.com/opendatahub-io/odh-platform-utilities/pkg/metadata/labels"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: mcp-lifecycle-operator-system
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: controller-manager
  namespace: mcp-lifecycle-operator-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: mcp-lifecycle-operator-system
spec:
  template:
    spec:
      containers:
      - name: manager
        image: original:latest
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: manager-role
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: mcp-lifecycle-operator-system
`

func newTestFS(yaml string) fstest.MapFS {
	return fstest.MapFS{
		"resources/mcp-lifecycle-operator.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}
}

func TestKustomizeProviderManifests(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifest))

	resources, err := provider.Manifests(context.Background(), Params{
		OperandNamespace: "custom-ns",
		OperandImage:     "my-registry/my-image:v1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resources) != 4 {
		t.Fatalf("expected 4 resources, got %d", len(resources))
	}

	for _, obj := range resources {
		labels := obj.GetLabels()
		if labels[odhLabels.PlatformPartOf] != v1alpha1.MCPLifecycleOperatorServiceName {
			t.Errorf("resource %s/%s missing part-of label", obj.GetKind(), obj.GetName())
		}
	}
}

func TestReplaceNamespace(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifest))

	resources, err := provider.Manifests(context.Background(), Params{
		OperandNamespace: "target-ns",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		switch obj.GetKind() {
		case "Namespace":
			if obj.GetName() != "target-ns" {
				t.Errorf("Namespace name = %q, want %q", obj.GetName(), "target-ns")
			}
		case "ServiceAccount", "Deployment":
			if obj.GetNamespace() != "target-ns" {
				t.Errorf("%s namespace = %q, want %q", obj.GetKind(), obj.GetNamespace(), "target-ns")
			}
		}
	}
}

func TestReplaceImage(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifest))

	resources, err := provider.Manifests(context.Background(), Params{
		OperandImage: "new-image:v2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			t.Fatalf("deployment containers missing or invalid: found=%v err=%v", found, err)
		}
		managerFound := false
		for _, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				t.Fatalf("container has unexpected type: %T", c)
			}
			if container["name"] == "manager" {
				managerFound = true
				if container["image"] != "new-image:v2" {
					t.Errorf("manager image = %q, want %q", container["image"], "new-image:v2")
				}
			}
		}
		if !managerFound {
			t.Fatal("manager container not found")
		}
	}
}

func TestEmptyImageSkipsReplacement(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifest))

	resources, err := provider.Manifests(context.Background(), Params{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			t.Fatalf("deployment containers missing or invalid: found=%v err=%v", found, err)
		}
		for _, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				t.Fatalf("container has unexpected type: %T", c)
			}
			if container["name"] == "manager" {
				if container["image"] != "original:latest" {
					t.Errorf("manager image = %q, want %q (should not be replaced)", container["image"], "original:latest")
				}
			}
		}
	}
}

func TestDefaultNamespaceUsedWhenEmpty(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifest))

	resources, err := provider.Manifests(context.Background(), Params{
		OperandNamespace: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() == "ServiceAccount" {
			if obj.GetNamespace() != DefaultOperandNamespace {
				t.Errorf("ServiceAccount namespace = %q, want %q", obj.GetNamespace(), DefaultOperandNamespace)
			}
		}
	}
}

func TestMissingOperandYAML(t *testing.T) {
	provider := NewKustomizeProvider(fstest.MapFS{})

	_, err := provider.Manifests(context.Background(), Params{})
	if err == nil {
		t.Fatal("expected error for missing operand.yaml, got nil")
	}
}
