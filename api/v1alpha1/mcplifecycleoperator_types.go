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

package v1alpha1

import (
	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	MCPLifecycleOperatorServiceName  = "mcplifecycleoperator"
	MCPLifecycleOperatorInstanceName = "default"
	MCPLifecycleOperatorKind         = "MCPLifecycleOperator"
)

var _ platformcommon.PlatformObject = (*MCPLifecycleOperator)(nil)

// Distribution describes the platform distribution context the module is
// currently aligned to, per the module onboarding guide.
// +kubebuilder:object:generate=true
type Distribution struct {
	// Name is the distribution name (e.g. SelfManagedRHOAI, OpenDataHub, Standalone).
	Name string `json:"name,omitempty"`
	// Version is the distribution version (e.g. 3.5.1, 0.0.0).
	Version string `json:"version,omitempty"`
}

// MCPLifecycleOperatorSpec defines the desired state of MCPLifecycleOperator.
type MCPLifecycleOperatorSpec struct {
	platformcommon.ManagementSpec `json:",inline"`
}

// MCPLifecycleOperatorStatus defines the observed state of MCPLifecycleOperator.
type MCPLifecycleOperatorStatus struct {
	platformcommon.Status `json:",inline"`

	platformcommon.ComponentReleaseStatus `json:",inline"`

	// Distribution is the platform distribution context the module is currently aligned to.
	Distribution Distribution `json:"distribution,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="MCPLifecycleOperator name must be default"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"

// MCPLifecycleOperator is the Schema for the mcplifecycleoperators API.
type MCPLifecycleOperator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPLifecycleOperatorSpec   `json:"spec,omitempty"`
	Status MCPLifecycleOperatorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MCPLifecycleOperatorList contains a list of MCPLifecycleOperator.
type MCPLifecycleOperatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPLifecycleOperator `json:"items"`
}

func (m *MCPLifecycleOperator) GetStatus() *platformcommon.Status {
	return &m.Status.Status
}

func (m *MCPLifecycleOperator) GetConditions() []platformcommon.Condition {
	return m.Status.Status.Conditions
}

func (m *MCPLifecycleOperator) SetConditions(conditions []platformcommon.Condition) {
	m.Status.Status.Conditions = conditions
}

func (m *MCPLifecycleOperator) GetReleaseStatus() *platformcommon.ComponentReleaseStatus {
	return &m.Status.ComponentReleaseStatus
}

func (m *MCPLifecycleOperator) SetReleaseStatus(s platformcommon.ComponentReleaseStatus) {
	m.Status.ComponentReleaseStatus = s
}

func init() {
	SchemeBuilder.Register(&MCPLifecycleOperator{}, &MCPLifecycleOperatorList{})
}
