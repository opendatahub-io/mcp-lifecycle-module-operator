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
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"
)

var restConfig *rest.Config
var k8sClient client.Client

const (
	operandDeployment        = "mcp-lifecycle-operator-controller-manager"
	operandCRD               = "mcpservers.mcp.x-k8s.io"
	moduleOperatorDeployment = "mcp-lifecycle-module-operator-controller-manager"

	timeout            = 5 * time.Minute
	interval           = 5 * time.Second
	consistentDuration = 30 * time.Second
	consistentInterval = 5 * time.Second
)

var operandNamespace string

var (
	suiteOnce  sync.Once
	suiteReady bool
	suiteOwner string
)

// prepareSuite connects to the prepared installation once per test binary. The
// first test to call it reports any setup failure; later tests fail with a
// pointer to that test instead of retrying setup or skipping.
func prepareSuite(t *testing.T) {
	t.Helper()
	suiteOnce.Do(func() {
		suiteOwner = t.Name()
		setupSuite(t)
		suiteReady = true
	})
	if !suiteReady {
		t.Fatalf("suite setup failed in %s; see its output", suiteOwner)
	}
}

func setupSuite(t *testing.T) {
	g := NewWithT(t)
	logf.SetLogger(logr.Discard())
	scheme := runtime.NewScheme()
	g.Expect(admissionv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(discoveryv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(networkingv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(extv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	g.Expect(err).NotTo(HaveOccurred())
	cfg.Timeout = 30 * time.Second
	restConfig = cfg

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	ctx := context.Background()
	t.Log("Discovering the prepared module installation namespace")
	operandNamespace, err = discoverSystemNamespace(ctx, k8sClient, os.Getenv("SYSTEM_NAMESPACE"))
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf("Using system namespace %q", operandNamespace)

	t.Log("Checking the prepared module installation and Managed singleton")
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, &appsv1.Deployment{})).To(Succeed())
	cr := &v1alpha1.MCPLifecycleOperator{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, cr)).To(Succeed(), "create MCPLifecycleOperator/default before running tests")
	g.Expect(cr.DeletionTimestamp).To(BeNil())
	g.Expect(cr.Spec.ManagementState).To(Equal(platformcommon.Managed), "prepare the singleton as Managed before running tests")
	waitForOperandReady(t, ctx)
}

// prepareTest connects to the installation and waits for a ready operand.
func prepareTest(t *testing.T) {
	t.Helper()
	prepareSuite(t)
	waitForOperandReady(t, context.Background())
}

// requireUnownedSingleton skips tests that delete the singleton when a parent
// owner (such as RHOAI) manages it and would recreate or fight over it.
func requireUnownedSingleton(t *testing.T) {
	t.Helper()
	cr := &v1alpha1.MCPLifecycleOperator{}
	NewWithT(t).Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "default"}, cr)).To(Succeed())
	if len(cr.OwnerReferences) != 0 {
		t.Skip("standalone tests require an unowned MCPLifecycleOperator singleton; this installation has a parent owner")
	}
}

func createTestNamespace(t *testing.T, ctx context.Context, prefix string) string {
	t.Helper()
	g := NewWithT(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	t.Cleanup(func() {
		uid := namespace.UID
		err := k8sClient.Delete(ctx, namespace, client.Preconditions{UID: &uid})
		g.Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue(), "delete fixture namespace: %v", err)
		g.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: namespace.Name}, &corev1.Namespace{}))
		}, timeout, interval).Should(BeTrue(), "fixture namespace %s still exists", namespace.Name)
	})
	return namespace.Name
}

func waitForOperandReady(t *testing.T, ctx context.Context) {
	t.Helper()
	g := NewWithT(t)
	t.Log("Waiting for the operand namespace to be created")
	g.Eventually(func(g Gomega) {
		ns := &corev1.Namespace{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	t.Log("Waiting for the operand CRD to be installed")
	g.Eventually(func(g Gomega) {
		crd := &extv1.CustomResourceDefinition{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	t.Log("Waiting for the operand rollout and readiness propagation")
	g.Eventually(func() error { return checkReadiness(ctx) }, timeout, interval).Should(Succeed())
}
