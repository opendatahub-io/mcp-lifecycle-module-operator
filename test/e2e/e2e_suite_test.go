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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
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

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MCPLMO E2E Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(GinkgoLogr)
	config, _ := GinkgoConfiguration()
	Expect(config.ParallelTotal).To(Equal(1), "MCPLMO tests require one Ginkgo process and exclusive access to the MCP component")
	scheme := runtime.NewScheme()
	Expect(admissionv1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(discoveryv1.AddToScheme(scheme)).To(Succeed())
	Expect(networkingv1.AddToScheme(scheme)).To(Succeed())
	Expect(extv1.AddToScheme(scheme)).To(Succeed())
	Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	cfg.Timeout = 30 * time.Second
	restConfig = cfg

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	ctx := context.Background()
	By("Discovering the prepared module installation namespace")
	operandNamespace, err = discoverSystemNamespace(ctx, k8sClient, os.Getenv("SYSTEM_NAMESPACE"))
	Expect(err).NotTo(HaveOccurred())
	GinkgoWriter.Printf("Using system namespace %q\n", operandNamespace)

	By("Checking the prepared module installation and Managed singleton")
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, &appsv1.Deployment{})).To(Succeed())
	cr := &v1alpha1.MCPLifecycleOperator{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, cr)).To(Succeed(), "create MCPLifecycleOperator/default before running tests")
	Expect(cr.DeletionTimestamp).To(BeNil())
	Expect(cr.Spec.ManagementState).To(Equal(platformcommon.Managed), "prepare the singleton as Managed before running tests")
	waitForOperandReady(ctx)
})

func prepareTest() {
	waitForOperandReady(context.Background())
}

func createTestNamespace(ctx context.Context, prefix string) string {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	DeferCleanup(func() {
		uid := namespace.UID
		err := k8sClient.Delete(ctx, namespace, client.Preconditions{UID: &uid})
		Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue(), "delete fixture namespace: %v", err)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: namespace.Name}, &corev1.Namespace{}))
		}, timeout, interval).Should(BeTrue(), "fixture namespace %s still exists", namespace.Name)
	})
	return namespace.Name
}

func waitForOperandReady(ctx context.Context) {
	By("Waiting for the operand namespace to be created")
	Eventually(func(g Gomega) {
		ns := &corev1.Namespace{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand CRD to be installed")
	Eventually(func(g Gomega) {
		crd := &extv1.CustomResourceDefinition{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand rollout and readiness propagation")
	Eventually(func() error { return checkReadiness(ctx) }, timeout, interval).Should(Succeed())
}
