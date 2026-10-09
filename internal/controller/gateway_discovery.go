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
	"encoding/json"
	"fmt"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

const (
	mcpGatewayExtensionCRDName = "mcpgatewayextensions.mcp.kuadrant.io"
	gatewayCRDName             = "gateways.gateway.networking.k8s.io"
)

var (
	mcpGatewayExtensionGVR = schema.GroupVersionResource{
		Group:    "mcp.kuadrant.io",
		Version:  "v1",
		Resource: "mcpgatewayextensions",
	}

	gatewayGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
)

type mcpGatewayExtensionTargetRef struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	SectionName string `json:"sectionName"`
}

type mcpGatewayExtensionSpec struct {
	TargetRef mcpGatewayExtensionTargetRef `json:"targetRef"`
}

type mcpGatewayExtensionStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type mcpGatewayExtension struct {
	Spec   mcpGatewayExtensionSpec   `json:"spec"`
	Status mcpGatewayExtensionStatus `json:"status"`
}

func (r *MCPLifecycleOperatorReconciler) crdAvailable(ctx context.Context, name string) (bool, error) {
	crd := &extv1.CustomResourceDefinition{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
		if k8serr.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (r *MCPLifecycleOperatorReconciler) discoverMCPGateways(ctx context.Context) ([]v1alpha1.MCPGatewayInfo, error) {
	log := logf.FromContext(ctx)

	available, err := r.crdAvailable(ctx, mcpGatewayExtensionCRDName)
	if err != nil {
		return nil, fmt.Errorf("checking MCPGatewayExtension CRD availability: %w", err)
	}

	if !available {
		return nil, nil
	}

	list, err := r.DynamicClient.Resource(mcpGatewayExtensionGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing MCPGatewayExtensions: %w", err)
	}

	if len(list.Items) == 0 {
		return nil, nil
	}

	var result []v1alpha1.MCPGatewayInfo

	for i := range list.Items {
		item := &list.Items[i]

		raw, err := item.MarshalJSON()
		if err != nil {
			log.Error(err, "Failed to marshal MCPGatewayExtension", "name", item.GetName())
			continue
		}

		var ext mcpGatewayExtension
		if err := json.Unmarshal(raw, &ext); err != nil {
			log.Error(err, "Failed to unmarshal MCPGatewayExtension", "name", item.GetName())
			continue
		}

		gwNamespace := ext.Spec.TargetRef.Namespace
		if gwNamespace == "" {
			gwNamespace = item.GetNamespace()
		}

		info := v1alpha1.MCPGatewayInfo{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
			Ready:     isConditionTrue(ext.Status.Conditions, "Ready"),
			Gateway: v1alpha1.GatewayRef{
				Name:      ext.Spec.TargetRef.Name,
				Namespace: gwNamespace,
			},
		}

		listeners, err := r.resolveGatewayListeners(ctx, gwNamespace, ext.Spec.TargetRef.Name)
		if err != nil {
			return nil, err
		}

		info.Gateway.Listeners = listeners

		result = append(result, info)
	}

	return result, nil
}

func (r *MCPLifecycleOperatorReconciler) resolveGatewayListeners(ctx context.Context, namespace, name string) ([]v1alpha1.ListenerRef, error) {
	obj, err := r.DynamicClient.Resource(gatewayGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("getting Gateway %s/%s: %w", namespace, name, err)
	}

	raw, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshalling Gateway %s/%s: %w", namespace, name, err)
	}

	var gw gatewayv1.Gateway
	if err := json.Unmarshal(raw, &gw); err != nil {
		return nil, fmt.Errorf("unmarshalling Gateway %s/%s: %w", namespace, name, err)
	}

	// Only listeners that are usable for MCP HTTPRoute creation are included.
	// Wildcard listeners (e.g. *.mcp.local) work with any protocol and allow
	// per-server hostnames. Direct hostname listeners (e.g. mcp.example.com)
	// only work with HTTP because HTTPS requires wildcard hostnames for
	// per-server routing via TLS filter chains (see routing.md in mcp-gateway).
	// Listeners without a hostname are excluded as no route hostname can be
	// constructed from them.
	var listeners []v1alpha1.ListenerRef
	for _, l := range gw.Spec.Listeners {
		if l.Hostname == nil {
			continue
		}
		hostname := string(*l.Hostname)
		switch {
		case strings.HasPrefix(hostname, "*."):
		case l.Protocol == gatewayv1.HTTPProtocolType && hostname != "":
		default:
			continue
		}

		listeners = append(listeners, v1alpha1.ListenerRef{
			Name:     string(l.Name),
			Hostname: hostname,
		})
	}

	return listeners, nil
}

func isConditionTrue(conditions []metav1.Condition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}

	return false
}

func (r *MCPLifecycleOperatorReconciler) tryRegisterGatewayWatches(ctx context.Context) {
	log := logf.FromContext(ctx)

	if r.controller == nil || r.DynamicInformerFactory == nil {
		return
	}

	if available, err := r.crdAvailable(ctx, mcpGatewayExtensionCRDName); err == nil && available {
		r.watchMCPGEOnce.Do(func() {
			informer := r.DynamicInformerFactory.ForResource(mcpGatewayExtensionGVR).Informer()
			r.DynamicInformerFactory.Start(ctx.Done())

			if err := r.controller.Watch(&source.Informer{
				Informer: informer,
				Handler:  r.enqueueComponentCR,
			}); err != nil {
				log.Error(err, "Failed to watch MCPGatewayExtension resources")
			}
		})
	}

	if available, err := r.crdAvailable(ctx, gatewayCRDName); err == nil && available {
		r.watchGatewayOnce.Do(func() {
			informer := r.DynamicInformerFactory.ForResource(gatewayGVR).Informer()
			r.DynamicInformerFactory.Start(ctx.Done())

			if err := r.controller.Watch(&source.Informer{
				Informer: informer,
				Handler:  r.enqueueComponentCR,
			}); err != nil {
				log.Error(err, "Failed to watch Gateway resources")
			}
		})
	}
}
