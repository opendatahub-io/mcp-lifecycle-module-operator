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
	libconditions "github.com/opendatahub-io/odh-platform-utilities/pkg/controller/conditions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionMCPLifecycleOperatorAvailable = "MCPLifecycleOperatorAvailable"

	// ConditionMCPServerStorageMigrated reports the outcome of the stored
	// MCPServer storage-version migration to v1beta1. It is intentionally
	// EXCLUDED from featureConditionTypes below so AggregateReady never folds
	// it into Ready/Degraded/ProvisioningSucceeded: a pending, running, or
	// failed migration is observable but never degrades operand readiness.
	ConditionMCPServerStorageMigrated = "MCPServerStorageMigrated"

	// ConditionMCPServerConversionVerified records that this controller has
	// verified stored MCPServer objects can be served at v1 and v1beta1. Its
	// reason distinguishes the current two-version check from the earlier
	// v1beta1-only check, so a freshly rolled-out controller rechecks conversion
	// even if the previous controller already marked this condition True and
	// advanced status.distribution. It is intentionally EXCLUDED from
	// featureConditionTypes so it never affects operand readiness; conversion
	// FAILURES are surfaced on MCPLifecycleOperatorAvailable instead.
	ConditionMCPServerConversionVerified = "MCPServerConversionVerified"
)

var featureConditionTypes = map[string]bool{
	ConditionMCPLifecycleOperatorAvailable: true,
}

// +kubebuilder:object:generate=false
type ConditionsManager struct {
	accessor   platformcommon.ConditionsAccessor
	generation int64
}

func NewConditionsManager(accessor platformcommon.ConditionsAccessor, generation int64) *ConditionsManager {
	return &ConditionsManager{
		accessor:   accessor,
		generation: generation,
	}
}

func (cm *ConditionsManager) MarkTrue(condType string) {
	libconditions.SetStatusCondition(cm.accessor, platformcommon.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Available",
		ObservedGeneration: cm.generation,
	})
}

// MarkTrueWithReason marks a condition True with an explicit reason. MarkTrue
// hardcodes reason "Available", which is wrong for signals other than operand
// availability (e.g. the migration-succeeded or conversion-verified markers).
func (cm *ConditionsManager) MarkTrueWithReason(condType, reason string) {
	libconditions.SetStatusCondition(cm.accessor, platformcommon.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		ObservedGeneration: cm.generation,
	})
}

func (cm *ConditionsManager) MarkFalse(condType, reason, message string) {
	libconditions.SetStatusCondition(cm.accessor, platformcommon.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cm.generation,
	})
}

func (cm *ConditionsManager) MarkUnknown(condType string) {
	libconditions.SetStatusCondition(cm.accessor, platformcommon.Condition{
		Type:               condType,
		Status:             metav1.ConditionUnknown,
		Reason:             "Progressing",
		ObservedGeneration: cm.generation,
	})
}

func (cm *ConditionsManager) AggregateReady() {
	opAvail := libconditions.FindStatusCondition(cm.accessor, ConditionMCPLifecycleOperatorAvailable)
	if opAvail == nil || opAvail.Status != metav1.ConditionTrue {
		cm.MarkFalse(string(platformcommon.ConditionTypeProvisioningSucceeded),
			"OperandDeploymentFailed", "MCP Lifecycle Operator operand is not available")
		cm.MarkFalse(string(platformcommon.ConditionTypeReady),
			"OperandDeploymentFailed", "MCP Lifecycle Operator operand is not available")
		cm.MarkFalse(string(platformcommon.ConditionTypeDegraded), "NotDegraded", "")
		return
	}

	anyFailing := false
	anyUnknown := false

	for _, c := range cm.accessor.GetConditions() {
		if !featureConditionTypes[c.Type] {
			continue
		}
		switch c.Status {
		case metav1.ConditionFalse:
			if c.Severity != platformcommon.ConditionSeverityInfo {
				anyFailing = true
			}
		case metav1.ConditionUnknown:
			anyUnknown = true
		}
	}

	cm.MarkTrue(string(platformcommon.ConditionTypeProvisioningSucceeded))

	switch {
	case anyFailing:
		cm.MarkTrue(string(platformcommon.ConditionTypeReady))
		cm.MarkTrue(string(platformcommon.ConditionTypeDegraded))
	case anyUnknown:
		cm.MarkUnknown(string(platformcommon.ConditionTypeReady))
		cm.MarkFalse(string(platformcommon.ConditionTypeDegraded), "NotDegraded", "")
	default:
		cm.MarkTrue(string(platformcommon.ConditionTypeReady))
		cm.MarkFalse(string(platformcommon.ConditionTypeDegraded), "NotDegraded", "")
	}
}

func (cm *ConditionsManager) Phase() platformcommon.Phase {
	if libconditions.IsStatusConditionTrue(cm.accessor, string(platformcommon.ConditionTypeReady)) {
		return platformcommon.PhaseReady
	}
	return platformcommon.PhaseNotReady
}
