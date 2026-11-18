// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"maps"
	"slices"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Views detach historical checkpoint data; they convey no current API or
// physical authority. Freshness remains the consuming stage's responsibility.
type operationPreparationReceipts struct {
	BundleDigest    string
	Management      managementBootstrapReceipt
	TenantUID       types.UID
	TenantSystemUID string
	TenantSources   map[string]types.UID
	Enrollment      enrollmentStageReceipt
	Software        softwareVerificationReceipt
	Workload        []workloadResourceReceipt
}

func copyOperationManagement(value managementBootstrapReceipt) managementBootstrapReceipt {
	value.NamespaceUIDs = maps.Clone(value.NamespaceUIDs)
	return value
}
func copyOperationEnrollment(value enrollmentStageReceipt) enrollmentStageReceipt {
	value.Devices = slices.Clone(value.Devices)
	return value
}
func copyOperationSoftware(value softwareVerificationReceipt) softwareVerificationReceipt {
	value.Components = slices.Clone(value.Components)
	return value
}
func copyOperationProgress(value operationProgress) operationProgress {
	value.Sets, value.DPUs, value.Versions = maps.Clone(value.Sets), maps.Clone(value.DPUs), maps.Clone(value.Versions)
	return value
}
func savedProgress(value activationNodeProgress) operationProgress {
	return operationProgress{NodeName: value.nodeName, MaintenanceUID: value.maintenanceUID, ReleasedVersion: value.releasedVersion, ResetAcknowledged: value.resetAcknowledged, Sets: maps.Clone(value.sets), DPUs: maps.Clone(value.dpus), Versions: maps.Clone(value.versions)}
}
func copyOperationRequest(value ActivationRequest) ActivationRequest {
	value.BeforeSubmit, value.recordClaim, value.ResetSafetyExpiresAt = nil, nil, time.Time{}
	value.DPUNodes = slices.Clone(value.DPUNodes)
	for i := range value.DPUNodes {
		value.DPUNodes[i].SelectedDPUs = slices.Clone(value.DPUNodes[i].SelectedDPUs)
	}
	return value
}
func copyOperationObservation(value ActivationObservation) ActivationObservation {
	if value.KubernetesNodeReady != nil {
		ready := *value.KubernetesNodeReady
		value.KubernetesNodeReady = &ready
	}
	return value
}
func copyOperationReset(value operationReset) operationReset {
	value.Request = copyOperationRequest(value.Request)
	value.Rounds, value.ArmProofs = maps.Clone(value.Rounds), maps.Clone(value.ArmProofs)
	value.Observation = copyOperationObservation(value.Observation)
	return value
}
func copyOperationHost(value operationHost) operationHost {
	if value.Result != nil {
		result := *value.Result
		value.Result = &result
	}
	return value
}
func (o *deploymentOperation) preparationReceipts() operationPreparationReceipts {
	s := o.state
	return operationPreparationReceipts{BundleDigest: s.Binding.BundleDigest, Management: copyOperationManagement(s.Management), TenantUID: s.TenantUID, TenantSystemUID: s.TenantSystemUID, TenantSources: maps.Clone(s.TenantSources), Enrollment: copyOperationEnrollment(s.Enrollment), Software: copyOperationSoftware(s.Software), Workload: slices.Clone(s.Workload)}
}
func (o *deploymentOperation) deploymentUID() types.UID {
	if o == nil {
		return ""
	}
	return o.state.DeploymentUID
}
func (o *deploymentOperation) tenantBinding() (string, map[string]types.UID) {
	return o.state.TenantSystemUID, maps.Clone(o.state.TenantSources)
}
func (o *deploymentOperation) childBindings() map[string]activationObjectReceipt {
	return maps.Clone(o.state.Children)
}
func (o *deploymentOperation) enrolledDevices() []enrolledDeviceReceipt {
	return slices.Clone(o.state.Enrollment.Devices)
}
func (o *deploymentOperation) hostTransaction(serial string) (operationHost, bool) {
	value, known := o.state.Hosts[serial]
	return copyOperationHost(value), known
}
func (o *deploymentOperation) hostRequests() map[string]sshprobe.HostNetworkRequest {
	result := make(map[string]sshprobe.HostNetworkRequest, len(o.state.Hosts))
	for serial, host := range o.state.Hosts {
		result[serial] = host.Request
	}
	return result
}
func (o *deploymentOperation) hasHostRequest(serial string) bool {
	_, known := o.state.Hosts[serial]
	return known
}
func (o *deploymentOperation) reset(key string) (operationReset, bool) {
	value, known := o.state.Resets[key]
	return copyOperationReset(value), known
}
func (o *deploymentOperation) resetRoundHistory() []map[types.UID]operationRound {
	result := make([]map[types.UID]operationRound, 0, len(o.state.Resets))
	for _, reset := range o.state.Resets {
		result = append(result, maps.Clone(reset.Rounds))
	}
	return result
}

// Construction takes ownership without retaining caller-owned collections.
func newDeploymentOperation(store operationStore, object *corev1.ConfigMap, state operationState) *deploymentOperation {
	state.Management = copyOperationManagement(state.Management)
	state.Enrollment = copyOperationEnrollment(state.Enrollment)
	state.Software = copyOperationSoftware(state.Software)
	state.Workload = slices.Clone(state.Workload)
	state.TenantSources = maps.Clone(state.TenantSources)
	state.Children = maps.Clone(state.Children)
	state.HoldIntents = maps.Clone(state.HoldIntents)
	for key, value := range state.HoldIntents {
		state.HoldIntents[key] = copyOperationProgress(value)
	}
	state.Progress = maps.Clone(state.Progress)
	for key, value := range state.Progress {
		state.Progress[key] = copyOperationProgress(value)
	}
	state.Resets = maps.Clone(state.Resets)
	for key, value := range state.Resets {
		state.Resets[key] = copyOperationReset(value)
	}
	state.Hosts = maps.Clone(state.Hosts)
	for key, value := range state.Hosts {
		state.Hosts[key] = copyOperationHost(value)
	}
	return &deploymentOperation{store: store, object: object.DeepCopy(), state: state}
}

func (o *deploymentOperation) pendingResets() map[string]operationReset {
	result := make(map[string]operationReset)
	for key, value := range o.state.Resets {
		if !value.Acknowledged {
			result[key] = copyOperationReset(value)
		}
	}
	return result
}
