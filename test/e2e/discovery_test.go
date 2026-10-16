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
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func discoverSystemNamespace(ctx context.Context, c client.Reader, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	deployments := &appsv1.DeploymentList{}
	if err := c.List(ctx, deployments, client.MatchingLabels{
		"app.kubernetes.io/name": "mcp-lifecycle-module-operator",
		"control-plane":          "controller-manager",
	}); err != nil {
		return "", fmt.Errorf("listing module operator Deployments: %w", err)
	}
	var matches []string
	var namespace string
	for _, deployment := range deployments.Items {
		if deployment.DeletionTimestamp != nil {
			continue
		}
		namespace = deployment.Namespace
		matches = append(matches, deployment.Namespace+"/"+deployment.Name)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no module operator Deployment found; prepare an installation or set SYSTEM_NAMESPACE explicitly")
	case 1:
		return namespace, nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("multiple module operator Deployments found (%s); set SYSTEM_NAMESPACE explicitly", strings.Join(matches, ", "))
	}
}

func TestDiscoverSystemNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deployment := func(namespace string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      moduleOperatorDeployment,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "mcp-lifecycle-module-operator",
				"control-plane":          "controller-manager",
			},
		}}
	}
	terminating := deployment("old-installation")
	terminating.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	terminating.Finalizers = []string{"test.example/cleanup"}
	operand := deployment("operand-only")
	operand.Name = operandDeployment
	operand.Labels["app.kubernetes.io/name"] = "mcp-lifecycle-operator"
	listErr := errors.New("deployment listing denied")

	tests := []struct {
		name     string
		explicit string
		objects  []client.Object
		listErr  error
		want     string
		wantErr  string
	}{
		{name: "explicit namespace bypasses discovery", explicit: "selected-installation", listErr: listErr, want: "selected-installation"},
		{name: "Kind installation", objects: []client.Object{deployment("mcp-lifecycle-module-operator-system")}, want: "mcp-lifecycle-module-operator-system"},
		{name: "RHOAI installation", objects: []client.Object{deployment("redhat-ods-applications")}, want: "redhat-ods-applications"},
		{name: "custom namespace", objects: []client.Object{deployment("custom-applications")}, want: "custom-applications"},
		{name: "operand does not match", objects: []client.Object{operand}, wantErr: "no module operator Deployment found"},
		{name: "missing installation", wantErr: "no module operator Deployment found"},
		{name: "terminating installation is ignored", objects: []client.Object{terminating, deployment("active-installation")}, want: "active-installation"},
		{name: "multiple installations", objects: []client.Object{deployment("second"), deployment("first")}, wantErr: "multiple module operator Deployments found (first/" + moduleOperatorDeployment + ", second/" + moduleOperatorDeployment + "); set SYSTEM_NAMESPACE explicitly"},
		{name: "API error is propagated", listErr: listErr, wantErr: "listing module operator Deployments: deployment listing denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...)
			if tt.listErr != nil {
				builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return tt.listErr
					},
				})
			}
			got, err := discoverSystemNamespace(context.Background(), builder.Build(), tt.explicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if tt.listErr != nil && !errors.Is(err, tt.listErr) {
					t.Fatalf("error %v does not wrap API error %v", err, tt.listErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("namespace = %q, want %q", got, tt.want)
			}
		})
	}
}
