// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const enrollmentDeviceAPIVersion = "provisioning.dpu.nvidia.com/v1alpha1"

// validateEnrolledDevice binds DPF's deterministic DPUDevice object to the
// selected BMC. DPF 26.8 creates the device as lower(serial), stores the input
// BMC endpoint in spec.bmcIp/bmcPort, and stamps the factory-reset policy from
// DPUDiscovery. Status fields are optional during reconciliation; when present
// they must agree. requireStatus is used only after readiness has been reported.
func validateEnrolledDevice(object *unstructured.Unstructured, item enrollmentBMC, namespace string, requireStatus bool) error {
	serial := item.device.Serial
	ip, ipErr := netip.ParseAddr(item.device.BMCIP)
	if object == nil || ipErr != nil || !ip.Is4() || ip.Is4In6() || !ip.IsGlobalUnicast() || item.device.BMCPort < 1 || item.device.BMCPort > 65535 || item.device.PSID == "" || namespace == "" || serial == "" {
		return fmt.Errorf("selected DPU identity is incomplete")
	}
	if object.GetAPIVersion() != enrollmentDeviceAPIVersion || object.GetKind() != "DPUDevice" || object.GetName() != strings.ToLower(serial) || object.GetNamespace() != namespace || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
		return fmt.Errorf("DPUDevice identity is missing, deleting or differs from the selected device")
	}
	if err := rejectHardwareSkipLabel(object); err != nil {
		return err
	}
	specSerial, serialFound, serialErr := unstructured.NestedString(object.Object, "spec", "serialNumber")
	specIP, ipFound, ipErr := unstructured.NestedString(object.Object, "spec", "bmcIp")
	specPort, portFound, portErr := unstructured.NestedInt64(object.Object, "spec", "bmcPort")
	policy, policyFound, policyErr := unstructured.NestedString(object.Object, "spec", "bmcFactoryResetPolicy")
	credential, credentialFound, credentialErr := unstructured.NestedString(object.Object, "spec", "bmcCredentialSecretName")
	if serialErr != nil || ipErr != nil || portErr != nil || policyErr != nil || credentialErr != nil || !serialFound || !ipFound || !portFound || !policyFound || specSerial != serial || specIP != ip.String() || specPort != int64(item.device.BMCPort) || policy != "Never" || credentialFound && credential != "" {
		return fmt.Errorf("DPUDevice spec does not match the selected endpoint, serial and Never shared-credential contract")
	}
	if specPSID, found, err := unstructured.NestedString(object.Object, "spec", "psid"); err != nil || found && !strings.EqualFold(specPSID, item.device.PSID) {
		return fmt.Errorf("DPUDevice spec PSID conflicts with the selected hardware")
	}
	for _, field := range []struct {
		name string
		path []string
		want string
	}{{"serial", []string{"status", "serialNumber"}, serial}, {"IP", []string{"status", "bmcIp"}, ip.String()}} {
		got, found, err := unstructured.NestedString(object.Object, field.path...)
		if err != nil || found && got != field.want || requireStatus && !found {
			return fmt.Errorf("DPUDevice status %s does not match the selected hardware", field.name)
		}
	}
	statusPort, portFound, portErr := unstructured.NestedInt64(object.Object, "status", "bmcPort")
	if portErr != nil || portFound && statusPort != int64(item.device.BMCPort) || requireStatus && !portFound {
		return fmt.Errorf("DPUDevice status BMC port does not match the selected endpoint")
	}
	statusPSID, psidFound, psidErr := unstructured.NestedString(object.Object, "status", "psid")
	if psidErr != nil || psidFound && !strings.EqualFold(statusPSID, item.device.PSID) || requireStatus && (!psidFound || !strings.EqualFold(statusPSID, item.device.PSID)) {
		return fmt.Errorf("DPUDevice status PSID does not match the selected hardware")
	}
	dpuType, typeFound, typeErr := unstructured.NestedString(object.Object, "status", "dpuType")
	if typeErr != nil || typeFound && dpuType != "Unknown" && dpuType != "BlueField4" || requireStatus && (!typeFound || dpuType != "BlueField4") {
		return fmt.Errorf("DPUDevice status does not identify the selected BlueField-4 cohort")
	}
	return nil
}

// enrolledDeviceReadiness reports controller readiness only after the exact
// selected identity and required DPUDevice condition generations are current.
// The pinned DPF 26.8 condition writer attaches observedGeneration to these
// DPUDevice conditions; no controller message is copied into the result.
func enrolledDeviceReadiness(object *unstructured.Unstructured) Readiness {
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "No DPUDevice observation was provided.")
	}
	if err := rejectHardwareSkipLabel(object); err != nil {
		return result("DPU_DEVICE_HARDWARE_PROVISIONING_SKIPPED", "The DPUDevice is labeled to skip hardware initialization or provisioning.")
	}
	if terminal := terminalErrorReadiness(object, "DPUDevice"); terminal != nil {
		return *terminal
	}
	if readiness := EvaluateReadiness(object); !readiness.Ready {
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
	if err := validateEnrolledDeviceStatusShape(object); err != nil {
		return result("DPU_DEVICE_IDENTITY_INCOMPLETE", "DPUDevice status does not contain the selected serial, endpoint, PSID and BlueField-4 type.")
	}
	return ready("Selected DPUDevice identity and all required current initialization, credential, certificate, trust and factory-reset conditions are ready.")
}

func rejectHardwareSkipLabel(object *unstructured.Unstructured) error {
	value, found, err := unstructured.NestedString(object.Object, "metadata", "labels", "provisioning.dpu.nvidia.com/skip-hw-provisioning")
	if err != nil || found && value == "true" {
		return fmt.Errorf("DPUDevice skips hardware initialization or provisioning")
	}
	return nil
}

func validateEnrolledDeviceStatusShape(object *unstructured.Unstructured) error {
	for _, path := range [][]string{{"status", "serialNumber"}, {"status", "bmcIp"}, {"status", "psid"}} {
		value, found, err := unstructured.NestedString(object.Object, path...)
		if err != nil || !found || value == "" {
			return fmt.Errorf("required device status is missing")
		}
	}
	port, found, err := unstructured.NestedInt64(object.Object, "status", "bmcPort")
	if err != nil || !found || port < 1 || port > 65535 {
		return fmt.Errorf("required device port status is invalid")
	}
	typeName, found, err := unstructured.NestedString(object.Object, "status", "dpuType")
	if err != nil || !found || typeName != "BlueField4" {
		return fmt.Errorf("required DPU type status is invalid")
	}
	return nil
}

// validateEnrolledNode binds DPF's deterministic DPUNode to its selected
// DPUDevice. The pinned Ready condition has no observedGeneration field, so
// freshness is established from UID/spec identity plus the controller report.
func validateEnrolledNode(object *unstructured.Unstructured, item enrollmentBMC, namespace string) error {
	if object == nil || namespace == "" || item.device.Serial == "" {
		return fmt.Errorf("selected DPUNode identity is incomplete")
	}
	wantName := "dpu-node-" + strings.ToLower(item.device.Serial)
	if object.GetAPIVersion() != enrollmentDeviceAPIVersion || object.GetKind() != "DPUNode" || object.GetName() != wantName || object.GetNamespace() != namespace || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
		return fmt.Errorf("DPUNode identity is missing, deleting or differs from the selected device")
	}
	refs, found, err := unstructured.NestedSlice(object.Object, "spec", "dpus")
	if err != nil || !found || len(refs) != 1 {
		return fmt.Errorf("DPUNode device association is missing or ambiguous")
	}
	ref, ok := refs[0].(map[string]any)
	if !ok || ref["name"] != strings.ToLower(item.device.Serial) {
		return fmt.Errorf("DPUNode does not refer to the selected DPUDevice")
	}
	method, found, err := unstructured.NestedMap(object.Object, "spec", "nodeRebootMethod")
	if err != nil || !found || len(method) != 1 {
		return fmt.Errorf("DPUNode reboot method differs from the pinned Redfish discovery contract")
	}
	external, found := method["external"]
	externalFields, ok := external.(map[string]any)
	if !found || !ok || len(externalFields) != 0 {
		return fmt.Errorf("DPUNode must use only the external reboot method created by the pinned Redfish crawler")
	}
	return nil
}

func enrolledNodeReadiness(object *unstructured.Unstructured) Readiness {
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
