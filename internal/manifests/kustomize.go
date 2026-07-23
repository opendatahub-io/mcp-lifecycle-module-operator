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
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"

	"github.com/manifestival/manifestival"
	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	odhLabels "github.com/opendatahub-io/odh-platform-utilities/pkg/metadata/labels"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	operandYAMLPath         = "resources/mcp-lifecycle-operator.yaml"
	DefaultOperandNamespace = "mcp-lifecycle-operator-system"
)

// KustomizeProvider loads pre-rendered Kustomize manifests from an embedded
// filesystem and patches them at runtime with the supplied parameters.
type KustomizeProvider struct {
	fs fs.FS
}

// NewKustomizeProvider creates a Provider that reads operand manifests from the
// given embedded filesystem. The FS must contain "resources/operand.yaml".
func NewKustomizeProvider(fsys fs.FS) *KustomizeProvider {
	return &KustomizeProvider{fs: fsys}
}

func (p *KustomizeProvider) Manifests(_ context.Context, params Params) ([]unstructured.Unstructured, error) {
	resources, err := p.loadResources()
	if err != nil {
		return nil, fmt.Errorf("loading operand manifests: %w", err)
	}

	manifest, err := manifestival.ManifestFrom(manifestival.Slice(resources))
	if err != nil {
		return nil, fmt.Errorf("creating manifest: %w", err)
	}

	targetNS := params.OperandNamespace
	if targetNS == "" {
		targetNS = DefaultOperandNamespace
	}

	manifest, err = manifest.Transform(
		injectLabels(map[string]string{
			odhLabels.PlatformPartOf: v1alpha1.MCPLifecycleOperatorServiceName,
		}),
		manifestival.InjectNamespace(targetNS),
		replaceImage(params.OperandImage),
		injectTLSEnvVars(params.TLSMinVersion, params.TLSCipherSuites),
	)
	if err != nil {
		return nil, fmt.Errorf("transforming manifests: %w", err)
	}

	return manifest.Resources(), nil
}

func (p *KustomizeProvider) loadResources() ([]unstructured.Unstructured, error) {
	data, err := fs.ReadFile(p.fs, operandYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", operandYAMLPath, err)
	}

	var resources []unstructured.Unstructured
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding YAML document: %w", err)
		}
		if obj.Object == nil {
			continue
		}
		resources = append(resources, obj)
	}

	return resources, nil
}

func injectLabels(labels map[string]string) manifestival.Transformer {
	return func(u *unstructured.Unstructured) error {
		existing := u.GetLabels()
		if existing == nil {
			existing = make(map[string]string)
		}
		for k, v := range labels {
			existing[k] = v
		}
		u.SetLabels(existing)
		return nil
	}
}

const (
	envTLSMinVersion   = "TLS_MIN_VERSION"
	envTLSCipherSuites = "TLS_CIPHER_SUITES"
	envPropagateTLS    = "PROPAGATE_TLS_ENV_VARS"
)

func injectTLSEnvVars(minVersion, cipherSuites string) manifestival.Transformer {
	return func(u *unstructured.Unstructured) error {
		if minVersion == "" && cipherSuites == "" {
			return nil
		}
		if u.GetKind() != "Deployment" {
			return nil
		}

		containers, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			return nil
		}

		injected := false
		for i, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(container, "name"); name != "manager" {
				continue
			}

			envSlice, _, _ := unstructured.NestedSlice(container, "env")
			envSlice = setEnvVar(envSlice, envTLSMinVersion, minVersion)
			envSlice = setEnvVar(envSlice, envTLSCipherSuites, cipherSuites)
			envSlice = setEnvVar(envSlice, envPropagateTLS, "true")

			if err := unstructured.SetNestedSlice(container, envSlice, "env"); err != nil {
				return fmt.Errorf("deployment %q: setting TLS env vars: %w", u.GetName(), err)
			}
			containers[i] = container
			injected = true
		}

		if !injected {
			return fmt.Errorf("deployment %q has no container named %q to inject TLS env vars", u.GetName(), "manager")
		}

		return unstructured.SetNestedSlice(u.Object, containers, "spec", "template", "spec", "containers")
	}
}

func setEnvVar(envSlice []interface{}, name, value string) []interface{} {
	for i, e := range envSlice {
		env, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _, _ := unstructured.NestedString(env, "name"); n == name {
			env["value"] = value
			envSlice[i] = env
			return envSlice
		}
	}

	return append(envSlice, map[string]interface{}{
		"name":  name,
		"value": value,
	})
}

func replaceImage(newImage string) manifestival.Transformer {
	return func(u *unstructured.Unstructured) error {
		if newImage == "" || u.GetKind() != "Deployment" {
			return nil
		}

		containers, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("deployment %q: reading containers: %w", u.GetName(), err)
		}
		if !found {
			return fmt.Errorf("deployment %q is missing spec.template.spec.containers", u.GetName())
		}

		replaced := false
		for i, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(container, "name"); name == "manager" {
				container["image"] = newImage
				containers[i] = container
				replaced = true
			}
		}

		if !replaced {
			return fmt.Errorf("deployment %q has no container named %q to replace image", u.GetName(), "manager")
		}

		return unstructured.SetNestedSlice(u.Object, containers, "spec", "template", "spec", "containers")
	}
}
