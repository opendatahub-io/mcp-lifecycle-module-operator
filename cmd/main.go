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

package main

import (
	"context"
	"flag"
	"os"

	configv1 "github.com/openshift/api/config/v1"
	utiltls "github.com/openshift/controller-runtime-common/pkg/tls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/opendatahub-io/odh-platform-utilities/pkg/deploy"
	odhLabels "github.com/opendatahub-io/odh-platform-utilities/pkg/metadata/labels"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
	"github.com/opendatahub-io/mcp-lifecycle-module-operator/internal/controller"
	"github.com/opendatahub-io/mcp-lifecycle-module-operator/internal/manifests"
)

var (
	scheme = runtime.NewScheme()

	// stripManagedFields removes managed fields before an object is committed to
	// the cache. It is reused by stripCRDSchema because a per-object Transform
	// overrides the cache's DefaultTransform, so CRDs would otherwise lose the
	// managed-field stripping applied to every other cached type.
	stripManagedFields = cache.TransformStripManagedFields()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(extv1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(configv1.Install(scheme))
}

// stripCRDSchema is a cache transform for CustomResourceDefinitions. The
// controller only reads a CRD's metadata.name (to detect the gateway CRDs and
// trigger watch registration), so the per-version OpenAPI v3 schema - by far the
// largest part of a CRD - is dropped before the object is committed to the
// cache. This keeps every CRD watchable (the gateway CRDs carry no managed-by
// label, so the CRD cache cannot be label-filtered) while avoiding holding every
// cluster CRD's full schema in memory. Managed fields are stripped as well,
// because a per-object Transform overrides the cache's DefaultTransform.
//
// Caution: because this mutates the cached copy, a cached CRD read (r.Get on a
// CustomResourceDefinition) returns a nil Spec.Versions[].Schema. Any future
// code that needs a CRD's schema, conversion, or served/storage version detail
// must read it uncached (e.g. via the API reader), not through the client cache.
func stripCRDSchema(obj interface{}) (interface{}, error) {
	obj, err := stripManagedFields(obj)
	if err != nil {
		return obj, err
	}

	crd, ok := obj.(*extv1.CustomResourceDefinition)
	if !ok {
		return obj, nil
	}

	for i := range crd.Spec.Versions {
		crd.Spec.Versions[i].Schema = nil
	}

	return crd, nil
}

func main() {
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := logf.Log.WithName("setup")

	podNamespace := os.Getenv("SYSTEM_NAMESPACE")
	if podNamespace == "" {
		log.Error(nil, "missing required environment variable", "name", "SYSTEM_NAMESPACE")
		os.Exit(1)
	}

	// Cluster-scoped resources (ClusterRole, ClusterRoleBinding) cannot be
	// namespace-scoped, so we filter by the managed-resource label to avoid
	// caching every such object in the cluster. CRDs are handled differently
	// (see the ByObject entry below): the gateway CRDs we watch carry no
	// managed-by label, so the CRD cache cannot be label-filtered and instead
	// strips each CRD's bulky schema to keep memory down.
	managedSelector := labels.SelectorFromSet(labels.Set{
		odhLabels.PlatformPartOf: v1alpha1.MCPLifecycleOperatorServiceName,
	})

	config := ctrl.GetConfigOrDie()
	metricsOptions, tlsConfig, err := controller.MetricsOptionsWithTLSConfig(context.Background(), config)
	if err != nil {
		log.Error(err, "unable to resolve metrics TLS options")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "mcp-lifecycle-module-operator-leader",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				podNamespace: {},
			},
			DefaultTransform: stripManagedFields,
			ByObject: map[client.Object]cache.ByObject{
				&v1alpha1.MCPLifecycleOperator{}:  {},
				&rbacv1.ClusterRole{}:             {Label: managedSelector},
				&rbacv1.ClusterRoleBinding{}:      {Label: managedSelector},
				&extv1.CustomResourceDefinition{}: {Transform: stripCRDSchema},
			},
		},
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	cfg := mgr.GetConfig()

	dynClient := dynamic.NewForConfigOrDie(cfg)
	discoveryClient := discovery.NewDiscoveryClientForConfigOrDie(cfg)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)

	deployer := deploy.NewDeployer(
		deploy.WithFieldOwner(v1alpha1.MCPLifecycleOperatorServiceName),
		deploy.WithMode(deploy.ModeSSA),
		deploy.WithCache(),
		deploy.WithApplyOrder(),
	)

	manifestProvider := manifests.NewKustomizeProvider(controller.ResourcesFS)

	operatorVersion := os.Getenv("OPERATOR_VERSION")
	if operatorVersion == "" {
		log.Error(nil, "missing required environment variable", "name", "OPERATOR_VERSION")
		os.Exit(1)
	}

	operandImage := os.Getenv("RELATED_IMAGE_ODH_MCP_LIFECYCLE_OPERATOR_IMAGE")
	if operandImage == "" {
		log.Info("RELATED_IMAGE_ODH_MCP_LIFECYCLE_OPERATOR_IMAGE not set, using default image from manifests")
	}

	reconciler := &controller.MCPLifecycleOperatorReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		Deployer:               deployer,
		DynamicClient:          dynClient,
		DiscoveryClient:        discoveryClient,
		DynamicInformerFactory: dynInformerFactory,
		ManifestProvider:       manifestProvider,
		OperatorVersion:        operatorVersion,
		PodNamespace:           podNamespace,
		OperandImage:           operandImage,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "MCPLifecycleOperator")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	signalCtx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	if tlsConfig.APIAvailable {
		watcher := &utiltls.SecurityProfileWatcher{
			Client:                    mgr.GetClient(),
			InitialTLSProfileSpec:     tlsConfig.ProfileSpec,
			InitialTLSAdherencePolicy: tlsConfig.Adherence,
			OnProfileChange: func(_ context.Context, _, _ configv1.TLSProfileSpec) {
				log.Info("TLS profile changed, restarting to reload metrics TLS configuration")
				cancel()
			},
			OnAdherencePolicyChange: func(_ context.Context, _, _ configv1.TLSAdherencePolicy) {
				log.Info("TLS adherence policy changed, restarting to reload metrics TLS configuration")
				cancel()
			},
		}
		if err := watcher.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up TLS profile watcher")
			os.Exit(1)
		}
	}

	log.Info("starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
