// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package deployment provides native DPF execution helpers and classifies
// controller-reported readiness from Kubernetes and DPF API status.
package deployment

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Readiness is the bounded result of evaluating one observed resource.
type Readiness struct {
	Ready      bool
	ReasonCode string
	Message    string
}

// EvaluateReadiness classifies controller-reported readiness from status fields
// produced by the pinned DPF 26.8 APIs and Kubernetes controllers. It does not
// prove desired/live equality or companion-resource convergence, and it does
// not mutate obj. In particular, DPUCluster freshness and tenant API readiness
// must be checked separately by the caller.
func EvaluateReadiness(obj *unstructured.Unstructured) Readiness {
	if obj == nil {
		return result("RESOURCE_UNAVAILABLE", "No resource observation was provided.")
	}
	if deletion := deletionReadiness(obj); deletion != nil {
		return *deletion
	}
	switch obj.GetKind() {
	case "DPFOperatorConfig":
		if obj.GetAPIVersion() != "operator.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPFOperatorConfig readiness requires operator.dpu.nvidia.com/v1alpha1.")
		}
		return observedReadyCondition(obj, "CONFIG_STATUS_STALE")
	case "DPUCluster":
		if obj.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPUCluster readiness requires provisioning.dpu.nvidia.com/v1alpha1.")
		}
		return dpuClusterReadiness(obj)
	case "DPUDiscovery":
		if obj.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPUDiscovery readiness requires provisioning.dpu.nvidia.com/v1alpha1.")
		}
		return dpuDiscoveryReadiness(obj)
	case "DPUDevice":
		if obj.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPUDevice readiness requires provisioning.dpu.nvidia.com/v1alpha1.")
		}
		if terminal := terminalErrorReadiness(obj, "DPUDevice"); terminal != nil {
			return *terminal
		}
		return conditionReadiness(obj, "Ready", "DEVICE_STATUS_STALE", true)
	case "DPU":
		if obj.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPU readiness requires provisioning.dpu.nvidia.com/v1alpha1.")
		}
		if terminal := terminalErrorReadiness(obj, "DPU"); terminal != nil {
			return *terminal
		}
		return phaseReadiness(obj, "DPU", "DPU_STATUS_STALE", "Ready", "Error", "Deleting", true)
	case "BlueFieldSoftware":
		if obj.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "BlueFieldSoftware readiness requires provisioning.dpu.nvidia.com/v1alpha1.")
		}
		if terminal := terminalErrorReadiness(obj, "BlueFieldSoftware"); terminal != nil {
			return *terminal
		}
		return phaseReadiness(obj, "BlueFieldSoftware", "SOFTWARE_STATUS_STALE", "Ready", "Error", "Deleting", false)
	case "DPUDeployment":
		if obj.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" {
			return result("RESOURCE_API_UNSUPPORTED", "DPUDeployment readiness requires svc.dpu.nvidia.com/v1alpha1.")
		}
		return observedReadyCondition(obj, "DEPLOYMENT_STATUS_STALE")
	case "Deployment":
		if obj.GetAPIVersion() != "apps/v1" {
			return result("RESOURCE_API_UNSUPPORTED", "Kubernetes Deployment readiness requires apps/v1.")
		}
		return kubernetesDeploymentReadiness(obj)
	case "Node":
		if obj.GetAPIVersion() != "v1" {
			return result("RESOURCE_API_UNSUPPORTED", "Kubernetes Node readiness requires v1.")
		}
		return conditionReadiness(obj, "Ready", "NODE_STATUS_STALE", false)
	default:
		return result("RESOURCE_KIND_UNSUPPORTED", "No readiness contract is defined for this resource kind.")
	}
}

func observedReadyCondition(obj *unstructured.Unstructured, staleCode string) Readiness {
	generation, ok := metadataGeneration(obj)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "The resource metadata generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil || !found || observed <= 0 {
		return result(staleCode, "The controller has not reported a valid observed generation.")
	}
	if observed < generation {
		return result(staleCode, "The controller status is older than the current resource generation.")
	}
	if observed > generation {
		return result("RESOURCE_STATUS_INVALID", "The controller observed generation is ahead of the resource generation.")
	}
	return conditionAtGeneration(obj, "Ready", generation, true)
}

func conditionReadiness(obj *unstructured.Unstructured, conditionType, staleCode string, requireConditionGeneration bool) Readiness {
	generation := int64(0)
	if requireConditionGeneration {
		var ok bool
		generation, ok = metadataGeneration(obj)
		if !ok {
			return result("RESOURCE_GENERATION_MISSING", "The resource metadata generation is missing or invalid.")
		}
	}
	condition, readiness := findCondition(obj, conditionType)
	if !readiness.Ready && readiness.ReasonCode != "" {
		return readiness
	}
	if condition == nil {
		return result("READINESS_CONDITION_MISSING", fmt.Sprintf("The %s condition is missing.", conditionType))
	}
	if requireConditionGeneration {
		if condition.observedGeneration == nil {
			return result(staleCode, fmt.Sprintf("The %s condition does not report an observed generation.", conditionType))
		}
		if *condition.observedGeneration < generation {
			return result(staleCode, fmt.Sprintf("The %s condition is older than the current resource generation.", conditionType))
		}
		if *condition.observedGeneration > generation {
			return result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition is ahead of the resource generation.", conditionType))
		}
	}
	return conditionResult(*condition, conditionType)
}

func conditionAtGeneration(obj *unstructured.Unstructured, conditionType string, generation int64, requireConditionGeneration bool) Readiness {
	condition, readiness := findCondition(obj, conditionType)
	if !readiness.Ready && readiness.ReasonCode != "" {
		return readiness
	}
	if condition == nil {
		return result("READINESS_CONDITION_MISSING", fmt.Sprintf("The %s condition is missing.", conditionType))
	}
	if requireConditionGeneration {
		if condition.observedGeneration == nil {
			return result("CONDITION_GENERATION_MISSING", fmt.Sprintf("The %s condition does not report an observed generation.", conditionType))
		}
		if *condition.observedGeneration < generation {
			return result("CONDITION_STATUS_STALE", fmt.Sprintf("The %s condition is older than the current resource generation.", conditionType))
		}
		if *condition.observedGeneration > generation {
			return result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition is ahead of the resource generation.", conditionType))
		}
	}
	return conditionResult(*condition, conditionType)
}

func conditionResult(condition condition, conditionType string) Readiness {
	switch condition.status {
	case "True":
		return ready(conditionType + " condition reports True.")
	case "False":
		if condition.reason == "Failure" || condition.reason == "Failed" {
			return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", fmt.Sprintf("The %s condition reports terminal failure.", conditionType)), condition.reason)
		}
		if condition.reason == "AwaitingDeletion" || condition.reason == "Deleting" {
			return withControllerReason(result("RESOURCE_DELETING", fmt.Sprintf("The %s condition reports deletion in progress.", conditionType)), condition.reason)
		}
		return withControllerReason(result("RESOURCE_NOT_READY", fmt.Sprintf("The %s condition is False.", conditionType)), condition.reason)
	case "Unknown":
		return withControllerReason(result("RESOURCE_READINESS_UNKNOWN", fmt.Sprintf("The %s condition is Unknown.", conditionType)), condition.reason)
	default:
		return result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition has an invalid status.", conditionType))
	}
}

// withControllerReason preserves only a short identifier-like status reason.
// Controller messages are intentionally excluded because they may contain
// arbitrary remote text such as signed URLs or credential-bearing details.
func withControllerReason(readiness Readiness, reason string) Readiness {
	if len(reason) == 0 || len(reason) > 128 || reason == "Success" {
		return readiness
	}
	for _, char := range reason {
		if !validControllerReasonCharacter(char) {
			return readiness
		}
	}
	readiness.Message += " Controller reason: " + reason + "."
	return readiness
}

func validControllerReasonCharacter(char rune) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '_', char == '.', char == '-':
		return true
	default:
		return false
	}
}

func dpuClusterReadiness(obj *unstructured.Unstructured) Readiness {
	phase, found, err := unstructured.NestedString(obj.Object, "status", "phase")
	if err != nil || !found || phase == "" {
		return result("RESOURCE_PHASE_MISSING", "The DPUCluster status phase is missing or invalid.")
	}
	switch phase {
	case "Failed":
		return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", "The DPUCluster phase is Failed."), findControllerReason(obj, "Ready"))
	case "NotReady":
		return withControllerReason(result("RESOURCE_NOT_READY", "The DPUCluster phase is NotReady."), findControllerReason(obj, "Ready"))
	case "Pending", "Creating":
		return result("RESOURCE_PROGRESSING", fmt.Sprintf("The DPUCluster phase is %s.", phase))
	case "Ready":
		// DPF 26.8's DPUCluster controller uses phase plus a metav1 Ready
		// condition. Its condition builder does not populate observedGeneration,
		// and DPUClusterStatus has no observedGeneration field. This reports the
		// controller's readiness; callers must separately check desired spec and
		// Kamaji TenantControlPlane state when they need freshness/convergence.
		readiness := dpuClusterConditionsReadiness(obj)
		if readiness.Ready {
			readiness.Message = "DPUCluster phase and Ready condition report readiness; the API does not report generation freshness."
		}
		return readiness
	default:
		return result("RESOURCE_PHASE_INVALID", "The DPUCluster status phase is unsupported.")
	}
}

func findControllerReason(obj *unstructured.Unstructured, conditionType string) string {
	condition, readiness := findCondition(obj, conditionType)
	if !readiness.Ready || condition == nil {
		return ""
	}
	return condition.reason
}

func dpuClusterConditionsReadiness(obj *unstructured.Unstructured) Readiness {
	conditions, readiness := optionalConditions(obj)
	if !readiness.Ready && readiness.ReasonCode != "" {
		return readiness
	}
	byType := make(map[string]condition, len(conditions))
	for _, item := range conditions {
		if _, exists := byType[item.typeName]; exists {
			return result("RESOURCE_STATUS_AMBIGUOUS", fmt.Sprintf("The %s condition appears more than once.", item.typeName))
		}
		byType[item.typeName] = item
	}
	if readyCondition, exists := byType["Ready"]; !exists {
		return result("READINESS_CONDITION_MISSING", "The DPUCluster Ready condition is missing.")
	} else if readyCondition.status != "True" {
		return conditionResult(readyCondition, "DPUCluster Ready")
	}
	created, exists := byType["Created"]
	if !exists {
		return result("CLUSTER_CREATION_UNOBSERVED", "The DPUCluster Created condition is missing.")
	}
	if created.status != "True" {
		return conditionResult(created, "DPUCluster Created")
	}
	for _, item := range conditions {
		switch item.typeName {
		case "Ready", "EtcdEncryptionRotationInProgress", "EtcdEncryptionRotationBlocked":
			continue
		}
		if item.status != "True" {
			return conditionResult(item, "DPUCluster "+item.typeName)
		}
	}
	return ready("DPUCluster phase and conditions report readiness; the API does not report generation freshness.")
}

func terminalErrorReadiness(obj *unstructured.Unstructured, resource string) *Readiness {
	_, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		invalid := result("RESOURCE_STATUS_INVALID", fmt.Sprintf("%s conditions are malformed.", resource))
		return &invalid
	}
	if !found {
		return nil
	}
	condition, readiness := findCondition(obj, "Error")
	if !readiness.Ready && readiness.ReasonCode != "" {
		return &readiness
	}
	if condition == nil || condition.status == "False" {
		return nil
	}
	if condition.status == "True" {
		reason := condition.reason
		if reason == "Success" {
			// DPF's generic condition helper labels any True condition with
			// Reason=Success, including an active Error=True condition. Prefer
			// the aggregate Ready reason when it identifies the failure state.
			reason = findControllerReason(obj, "Ready")
		}
		failed := withControllerReason(result("RESOURCE_TERMINAL_FAILURE", fmt.Sprintf("%s reports an active Error condition.", resource)), reason)
		return &failed
	}
	unknown := withControllerReason(result("RESOURCE_READINESS_UNKNOWN", fmt.Sprintf("%s Error condition is Unknown.", resource)), condition.reason)
	return &unknown
}

func dpuDiscoveryReadiness(obj *unstructured.Unstructured) Readiness {
	generation, ok := metadataGeneration(obj)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "The resource metadata generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil || !found || observed <= 0 {
		return result("DISCOVERY_SCAN_UNOBSERVED", "DPUDiscovery has not reported a successful scan for the current generation.")
	}
	if observed < generation {
		return result("DISCOVERY_SCAN_STALE", "The last DPUDiscovery scan predates the current specification.")
	}
	if observed > generation {
		return result("RESOURCE_STATUS_INVALID", "DPUDiscovery observed generation is ahead of the resource generation.")
	}
	lastScan, found, err := unstructured.NestedString(obj.Object, "status", "lastScanTime")
	if err != nil || !found {
		return result("DISCOVERY_SCAN_UNOBSERVED", "DPUDiscovery has no timestamp for its successful scan.")
	}
	if _, err := time.Parse(time.RFC3339Nano, lastScan); err != nil {
		return result("RESOURCE_STATUS_INVALID", "DPUDiscovery lastScanTime is malformed.")
	}
	foundDPUs, exists, err := unstructured.NestedInt64(obj.Object, "status", "foundDPUs")
	if err != nil || exists && foundDPUs < 0 {
		return result("RESOURCE_STATUS_INVALID", "DPUDiscovery foundDPUs is malformed.")
	}
	if !exists || foundDPUs == 0 {
		return result("DISCOVERY_NO_DPUS_FOUND", "The current successful DPUDiscovery scan found no DPU devices.")
	}
	return ready("DPUDiscovery completed a successful scan and found at least one DPU device.")
}

func phaseReadiness(obj *unstructured.Unstructured, resource, staleCode, readyPhase, failurePhase, deletingPhase string, requireReadyCondition bool) Readiness {
	generation, ok := metadataGeneration(obj)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "The resource metadata generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil || !found || observed <= 0 {
		return result(staleCode, fmt.Sprintf("%s has no valid observed generation.", resource))
	}
	if observed < generation {
		return result(staleCode, fmt.Sprintf("%s status is older than the current resource generation.", resource))
	}
	if observed > generation {
		return result("RESOURCE_STATUS_INVALID", fmt.Sprintf("%s observed generation is ahead of the resource generation.", resource))
	}
	phase, found, err := unstructured.NestedString(obj.Object, "status", "phase")
	if err != nil || !found || phase == "" {
		return result("RESOURCE_PHASE_MISSING", fmt.Sprintf("%s status phase is missing or invalid.", resource))
	}
	if phase == failurePhase {
		return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", fmt.Sprintf("%s phase is %s.", resource, phase)), findControllerReason(obj, "Ready"))
	}
	if phase == deletingPhase {
		return result("RESOURCE_DELETING", fmt.Sprintf("%s phase is %s.", resource, phase))
	}
	if phase != readyPhase {
		return result("RESOURCE_PROGRESSING", fmt.Sprintf("%s has not reached its ready phase.", resource))
	}
	if requireReadyCondition {
		condition, readiness := findCondition(obj, "Ready")
		if !readiness.Ready && readiness.ReasonCode != "" {
			return readiness
		}
		if condition == nil {
			return result("READINESS_CONDITION_MISSING", fmt.Sprintf("%s Ready condition is missing.", resource))
		}
		if condition.observedGeneration != nil && *condition.observedGeneration != generation {
			return result("CONDITION_STATUS_STALE", fmt.Sprintf("%s Ready condition is not current.", resource))
		}
		if condition.status != "True" {
			return conditionResult(*condition, resource+" Ready")
		}
	}
	return ready(fmt.Sprintf("%s status phase is %s for the current generation.", resource, readyPhase))
}

func kubernetesDeploymentReadiness(obj *unstructured.Unstructured) Readiness {
	generation, ok := metadataGeneration(obj)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "The Deployment metadata generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil || !found || observed <= 0 {
		return result("DEPLOYMENT_STATUS_STALE", "The Deployment controller has not reported its observed generation.")
	}
	if observed < generation {
		return result("DEPLOYMENT_STATUS_STALE", "The Deployment status is older than the current specification.")
	}
	if observed > generation {
		return result("RESOURCE_STATUS_INVALID", "Deployment observed generation is ahead of the resource generation.")
	}
	desired, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if err != nil {
		return result("RESOURCE_SPEC_INVALID", "Deployment spec.replicas is malformed.")
	}
	if !found {
		desired = 1 // Kubernetes defaults an omitted Deployment replica count to one.
	}
	if desired < 0 {
		return result("RESOURCE_SPEC_INVALID", "Deployment spec.replicas cannot be negative.")
	}
	if desired == 0 {
		return result("DEPLOYMENT_SCALED_TO_ZERO", "A Deployment with zero desired replicas does not establish an available dependency.")
	}
	conditions, readiness := optionalConditions(obj)
	if !readiness.Ready && readiness.ReasonCode != "" {
		return readiness
	}
	for _, condition := range conditions {
		if condition.typeName == "Progressing" && condition.status == "False" && condition.reason == "ProgressDeadlineExceeded" {
			return result("DEPLOYMENT_ROLLOUT_FAILED", "The Deployment exceeded its progress deadline.")
		}
	}
	for _, field := range []string{"updatedReplicas", "readyReplicas", "availableReplicas"} {
		count, present, err := unstructured.NestedInt64(obj.Object, "status", field)
		if err != nil || present && count < 0 {
			return result("RESOURCE_STATUS_INVALID", "Deployment replica status is malformed.")
		}
		if !present {
			count = 0
		}
		if count != desired {
			return result("DEPLOYMENT_ROLLOUT_INCOMPLETE", "Deployment updated, ready, and available replica counts do not match the desired replica count.")
		}
	}
	return ready("Deployment controller observed the current generation and all desired replicas are updated, ready, and available.")
}

type condition struct {
	typeName           string
	status             string
	reason             string
	observedGeneration *int64
}

func findCondition(obj *unstructured.Unstructured, conditionType string) (*condition, Readiness) {
	conditions, readiness := listConditions(obj)
	if !readiness.Ready && readiness.ReasonCode != "" {
		return nil, readiness
	}
	var found *condition
	for i := range conditions {
		if conditions[i].typeName != conditionType {
			continue
		}
		if found != nil {
			return nil, result("RESOURCE_STATUS_AMBIGUOUS", fmt.Sprintf("The %s condition appears more than once.", conditionType))
		}
		copy := conditions[i]
		found = &copy
	}
	return found, Readiness{Ready: true}
}

func listConditions(obj *unstructured.Unstructured) ([]condition, Readiness) {
	items, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil, result("RESOURCE_CONDITIONS_MISSING", "The resource status conditions are missing or malformed.")
	}
	return parseConditions(items)
}

func optionalConditions(obj *unstructured.Unstructured) ([]condition, Readiness) {
	items, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return nil, result("RESOURCE_STATUS_INVALID", "The resource status conditions are malformed.")
	}
	if !found {
		return []condition{}, Readiness{Ready: true}
	}
	return parseConditions(items)
}

func parseConditions(items []interface{}) ([]condition, Readiness) {
	conditions := make([]condition, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]interface{})
		if !ok {
			return nil, result("RESOURCE_STATUS_INVALID", "A resource status condition is malformed.")
		}
		typeName, ok := fields["type"].(string)
		if !ok || len(typeName) > 316 || len(kvalidation.IsQualifiedName(typeName)) != 0 {
			return nil, result("RESOURCE_STATUS_INVALID", "A resource status condition has an invalid type.")
		}
		status, ok := fields["status"].(string)
		if !ok || status != "True" && status != "False" && status != "Unknown" {
			return nil, result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition has an invalid status value.", typeName))
		}
		condition := condition{typeName: typeName, status: status}
		if rawReason, exists := fields["reason"]; exists {
			var ok bool
			condition.reason, ok = rawReason.(string)
			if !ok {
				return nil, result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition has a malformed reason.", typeName))
			}
		}
		if rawObserved, exists := fields["observedGeneration"]; exists {
			value, ok := rawObserved.(int64)
			if !ok || value < 0 {
				return nil, result("RESOURCE_STATUS_INVALID", fmt.Sprintf("The %s condition has a malformed observed generation.", typeName))
			}
			condition.observedGeneration = &value
		}
		conditions = append(conditions, condition)
	}
	return conditions, Readiness{Ready: true}
}

func metadataGeneration(obj *unstructured.Unstructured) (int64, bool) {
	generation, found, err := unstructured.NestedInt64(obj.Object, "metadata", "generation")
	return generation, err == nil && found && generation > 0
}

func ready(message string) Readiness {
	return Readiness{Ready: true, ReasonCode: "RESOURCE_READY", Message: message}
}

func result(code, message string) Readiness {
	return Readiness{ReasonCode: code, Message: message}
}

func deletionReadiness(obj *unstructured.Unstructured) *Readiness {
	timestamp, found, err := unstructured.NestedString(obj.Object, "metadata", "deletionTimestamp")
	if err != nil {
		invalid := result("RESOURCE_METADATA_INVALID", "The resource deletion timestamp is malformed.")
		return &invalid
	}
	if !found {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		invalid := result("RESOURCE_METADATA_INVALID", "The resource deletion timestamp is malformed.")
		return &invalid
	}
	deleting := result("RESOURCE_DELETING", "The resource is being deleted.")
	return &deleting
}
