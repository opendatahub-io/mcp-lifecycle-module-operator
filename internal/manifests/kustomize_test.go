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

const testManifestWithEnvVars = `apiVersion: apps/v1
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
        env:
        - name: GOMEMLIMIT
          value: "120586240"
`

func TestInjectTLSEnvVars(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifestWithEnvVars))

	resources, err := provider.Manifests(context.Background(), Params{
		TLSMinVersion:   "VersionTLS12",
		TLSCipherSuites: "TLS_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			container := c.(map[string]interface{})
			if container["name"] != "manager" {
				continue
			}
			envSlice, _, _ := unstructured.NestedSlice(container, "env")
			envMap := make(map[string]string)
			for _, e := range envSlice {
				env := e.(map[string]interface{})
				envMap[env["name"].(string)] = env["value"].(string)
			}

			if envMap["GOMEMLIMIT"] != "120586240" {
				t.Error("existing GOMEMLIMIT env var was modified")
			}
			if envMap["TLS_MIN_VERSION"] != "VersionTLS12" {
				t.Errorf("TLS_MIN_VERSION = %q, want %q", envMap["TLS_MIN_VERSION"], "VersionTLS12")
			}
			if envMap["TLS_CIPHER_SUITES"] != "TLS_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256" {
				t.Errorf("TLS_CIPHER_SUITES = %q, want expected value", envMap["TLS_CIPHER_SUITES"])
			}
			if envMap["PROPAGATE_TLS_ENV_VARS"] != "true" {
				t.Errorf("PROPAGATE_TLS_ENV_VARS = %q, want 'true'", envMap["PROPAGATE_TLS_ENV_VARS"])
			}
		}
	}
}

func TestInjectTLSEnvVars_EmptyValues_SkipsInjection(t *testing.T) {
	provider := NewKustomizeProvider(newTestFS(testManifestWithEnvVars))

	resources, err := provider.Manifests(context.Background(), Params{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			container := c.(map[string]interface{})
			if container["name"] != "manager" {
				continue
			}
			envSlice, _, _ := unstructured.NestedSlice(container, "env")
			for _, e := range envSlice {
				env := e.(map[string]interface{})
				name := env["name"].(string)
				if name == "TLS_MIN_VERSION" || name == "TLS_CIPHER_SUITES" || name == "PROPAGATE_TLS_ENV_VARS" {
					t.Errorf("TLS env var %q should not be present when values are empty", name)
				}
			}
		}
	}
}

func TestInjectTLSEnvVars_NoManagerContainer_ReturnsError(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: mcp-lifecycle-operator-system
spec:
  template:
    spec:
      containers:
      - name: sidecar
        image: sidecar:latest
`
	provider := NewKustomizeProvider(newTestFS(manifest))

	_, err := provider.Manifests(context.Background(), Params{
		TLSMinVersion:   "VersionTLS12",
		TLSCipherSuites: "TLS_AES_128_GCM_SHA256",
	})
	if err == nil {
		t.Fatal("expected error when no manager container exists")
	}
}

func TestInjectTLSEnvVars_UpdatesExisting(t *testing.T) {
	manifest := `apiVersion: apps/v1
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
        env:
        - name: TLS_MIN_VERSION
          value: "VersionTLS10"
        - name: TLS_CIPHER_SUITES
          value: "old-cipher"
`
	provider := NewKustomizeProvider(newTestFS(manifest))

	resources, err := provider.Manifests(context.Background(), Params{
		TLSMinVersion:   "VersionTLS13",
		TLSCipherSuites: "TLS_AES_256_GCM_SHA384",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, obj := range resources {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			container := c.(map[string]interface{})
			if container["name"] != "manager" {
				continue
			}
			envSlice, _, _ := unstructured.NestedSlice(container, "env")
			envMap := make(map[string]string)
			for _, e := range envSlice {
				env := e.(map[string]interface{})
				envMap[env["name"].(string)] = env["value"].(string)
			}

			if envMap["TLS_MIN_VERSION"] != "VersionTLS13" {
				t.Errorf("TLS_MIN_VERSION = %q, want %q", envMap["TLS_MIN_VERSION"], "VersionTLS13")
			}
			if envMap["TLS_CIPHER_SUITES"] != "TLS_AES_256_GCM_SHA384" {
				t.Errorf("TLS_CIPHER_SUITES = %q, want %q", envMap["TLS_CIPHER_SUITES"], "TLS_AES_256_GCM_SHA384")
			}
		}
	}
}
