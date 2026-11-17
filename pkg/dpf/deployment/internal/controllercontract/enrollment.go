// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// EnrolledDeviceReadiness interprets current initialization, credential, certificate,
// trust and reset-policy conditions; selected hardware binding remains caller-owned.
func (r *Rules) EnrolledDeviceReadiness(object *unstructured.Unstructured) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "No DPUDevice observation was provided.")
	}
	if err := r.RejectHardwareSkipLabel(object); err != nil {
		return result("DPU_DEVICE_HARDWARE_PROVISIONING_SKIPPED", "The DPUDevice is labeled to skip hardware initialization or provisioning.")
	}
	if terminal := terminalErrorReadiness(object, "DPUDevice"); terminal != nil {
		return *terminal
	}
	if readiness := r.ResourceReadiness(object); !readiness.Ready {
		return readiness
	}
	for _, conditionType := range []string{"Initialized", "Discovered", "BMCCredentialsReady", "BMCServerCertificateReady", "CATrustBundleReady", "BMCFactoryResetReady"} {
		readiness := conditionReadiness(object, conditionType, "DEVICE_STATUS_STALE", true)
		if !readiness.Ready {
			return readiness
		}
	}
	reset, resetReadiness := findCondition(object, "BMCFactoryResetReady")
	if !resetReadiness.Ready || reset == nil {
		return resetReadiness
	}
	policy, found, err := unstructured.NestedString(object.Object, "spec", "bmcFactoryResetPolicy")
	if err != nil || !found || policy != "Never" || reset.reason != "FactoryResetSkipped" {
		return result("BMC_FACTORY_RESET_POLICY_UNVERIFIED", "DPUDevice has not confirmed the retained Never factory reset policy.")
	}
	if status := enrolledDeviceStatusReadiness(object); !status.Ready {
		return status
	}
	return ready("Selected DPUDevice identity and all required current initialization, credential, certificate, trust and factory-reset conditions are ready.")
}

func (r *Rules) RejectHardwareSkipLabel(object *unstructured.Unstructured) error {
	if !r.valid() || object == nil {
		return fmt.Errorf("selected controller rules and DPUDevice are required")
	}
	value, found, err := unstructured.NestedString(object.Object, "metadata", "labels", "provisioning.dpu.nvidia.com/skip-hw-provisioning")
	if err != nil || found && value == "true" {
		return fmt.Errorf("DPUDevice skips hardware initialization or provisioning")
	}
	return nil
}

func enrolledDeviceStatusReadiness(object *unstructured.Unstructured) Readiness {
	for _, field := range []struct{ name, code, description string }{
		{"serialNumber", "DPU_DEVICE_SERIAL", "the selected DPU serial number"},
		{"bmcIp", "DPU_DEVICE_BMC_IP", "the DPU BMC address"},
		{"psid", "DPU_DEVICE_PSID", "the PSID required to select the matching firmware bundle"},
	} {
		value, found, err := unstructured.NestedString(object.Object, "status", field.name)
		if err != nil {
			return result(field.code+"_INVALID", "The DPUDevice controller reported a non-string status."+field.name+"; it must identify "+field.description+".")
		}
		if !found || value == "" {
			return result(field.code+"_MISSING", "The DPUDevice controller has not reported "+field.description+" in status."+field.name+".")
		}
	}
	port, found, err := unstructured.NestedInt64(object.Object, "status", "bmcPort")
	if err != nil || !found || port < 1 || port > 65535 {
		return result("DPU_DEVICE_BMC_PORT_INVALID", "DPUDevice status.bmcPort must report the selected BMC port between 1 and 65535.")
	}
	typeName, found, err := unstructured.NestedString(object.Object, "status", "dpuType")
	if err != nil || !found || typeName != "BlueField4" {
		return result("DPU_DEVICE_TYPE_UNVERIFIED", "DPUDevice status.dpuType does not identify the selected BlueField-4 cohort.")
	}
	return ready("Required DPUDevice status fields are present.")
}

func (r *Rules) EnrolledNodeReadiness(object *unstructured.Unstructured) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "No DPUNode observation was provided.")
	}
	// DPF's Redfish crawler can reject the selected DPU details before the
	// DPUNode reconciler ever writes Ready. Check this terminal validation
	// condition first so a missing or False Ready condition cannot hide it.
	invalidDetails, invalidReadiness := findCondition(object, "InvalidDPUDetails")
	if !invalidReadiness.Ready {
		return invalidReadiness
	}
	if invalidDetails != nil {
		switch invalidDetails.status {
		case "True":
			return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", "The DPUNode reports invalid selected DPU details."), invalidDetails.reason)
		case "Unknown":
			return withControllerReason(result("RESOURCE_READINESS_UNKNOWN", "The DPUNode invalid-details condition is Unknown."), invalidDetails.reason)
		}
	}
	readyCondition, readiness := findCondition(object, "Ready")
	if !readiness.Ready {
		return readiness
	}
	if readyCondition == nil {
		return result("READINESS_CONDITION_MISSING", "The DPUNode Ready condition is missing.")
	}
	if readyCondition.status != "True" {
		return result("DPU_NODE_NOT_READY", "The DPUNode controller does not report Ready.")
	}
	for _, conditionType := range []string{"DPUNodeNodeEffectInProgress", "DPUUpdateInProgress", "DPUNodeRebootInProgress"} {
		condition, check := findCondition(object, conditionType)
		if !check.Ready {
			return check
		}
		if condition != nil && condition.status != "False" {
			return result("DPU_NODE_TRANSITION_ACTIVE", "A DPUNode validation, update, node effect or reboot transition is active.")
		}
	}
	// DPUNodeReconciler.updateDPUNodeStatusConditions in the pinned source sets
	// Type/Status/Reason/Message only; it does not populate observedGeneration.
	return ready("The selected DPUNode association is exact and its controller reports Ready.")
}
