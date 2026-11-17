// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"maps"
	"slices"

	"github.com/mitchellh/copystructure"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// controlPlaneVerificationInputs contains retained data only. In particular it
// cannot recover the operation store, first-create callback or tenant adapter
// from the prepared run or the nested workload receipt.
type controlPlaneVerificationInputs struct {
	execution      *managementVerificationInputs
	selected       []enrollmentBMC
	password       string
	management     managementBootstrapReceipt
	tenantUID      types.UID
	enrollment     enrollmentStageReceipt
	workloadDigest string
	software       softwareVerificationReceipt
	resources      []workloadResourceReceipt
}

func newControlPlaneVerificationInputs(prepared *preparedControlPlaneRun, control controlPlaneReceipts, workload workloadPreparationReceipt) *controlPlaneVerificationInputs {
	if prepared == nil {
		return nil
	}
	view := &controlPlaneVerificationInputs{
		selected: slices.Clone(prepared.selected), password: prepared.password,
		management: control.management, tenantUID: control.tenant.ClusterUID,
		enrollment: control.enrollment, workloadDigest: workload.BundleDigest,
		software: workload.Software, resources: slices.Clone(workload.Resources),
	}
	view.management.NamespaceUIDs = maps.Clone(control.management.NamespaceUIDs)
	view.enrollment.Devices = slices.Clone(control.enrollment.Devices)
	view.software.Components = slices.Clone(workload.Software.Components)
	// Access and private SSH buffers are immutable config-owned snapshots;
	// copy the SSH struct to isolate its exported Sudo setting.
	for i := range view.selected {
		view.selected[i].device.SelectorLabels = maps.Clone(view.selected[i].device.SelectorLabels)
		if view.selected[i].target.SSH != nil {
			access := *view.selected[i].target.SSH
			view.selected[i].target.SSH = &access
		}
	}
	if prepared.execution.verificationInputs() != nil {
		execution := *prepared.execution.verificationInputs()
		execution.namespaces = make([]*unstructured.Unstructured, len(prepared.execution.namespaces))
		for i, ns := range prepared.execution.namespaces {
			execution.namespaces[i] = ns.DeepCopy()
		}
		execution.configObject = execution.configObject.DeepCopy()
		execution.manager = execution.manager.DeepCopy()
		execution.crds = slices.Clone(execution.crds)
		for i := range execution.crds {
			execution.crds[i].versions = slices.Clone(execution.crds[i].versions)
			if execution.crds[i].spec != nil {
				execution.crds[i].spec = runtime.DeepCopyJSONValue(execution.crds[i].spec).(map[string]any)
			}
		}
		// Reconciler, selected rules and opaque metadata are immutable verification data.

		view.execution = &execution
	}
	return view
}

// workloadLeaseInputs retains only data and the exact bound tenant read port.
// This preserves the original tenant-instance equality check without retaining
// workload.ControlPlane's concrete client or any prepared-operation owner.
type workloadLeaseInputs struct {
	bundleDigest, managementDigest string
	tenantUID                      types.UID
	tenant                         tenantVerificationReader
	resources                      []workloadResourceReceipt
}

func newWorkloadLeaseInputs(workload workloadPreparationReceipt) workloadLeaseInputs {
	return workloadLeaseInputs{bundleDigest: workload.BundleDigest, managementDigest: workload.ControlPlane.management.BundleDigest,
		tenantUID: workload.ControlPlane.tenant.ClusterUID, tenant: workload.ControlPlane.tenant.Client, resources: slices.Clone(workload.Resources)}
}

func copyActivationVerificationReceipt(input activationStageReceipt) (activationStageReceipt, error) {
	output := input
	output.DPUSets = slices.Clone(input.DPUSets)
	output.Nodes = slices.Clone(input.Nodes)
	output.DPUs = slices.Clone(input.DPUs)
	output.Maintenance = slices.Clone(input.Maintenance)
	output.groups = maps.Clone(input.groups)
	for host, indexes := range input.groups {
		output.groups[host] = slices.Clone(indexes)
	}
	output.progress = copyActivationProgress(input.progress)
	output.Reboots = slices.Clone(input.Reboots)
	for i := range output.Reboots {
		output.Reboots[i].NodeNames = slices.Clone(input.Reboots[i].NodeNames)
		output.Reboots[i].completion = slices.Clone(input.Reboots[i].completion)
		for j := range output.Reboots[i].completion {
			output.Reboots[i].completion[j] = copyActivationNodeProgress(output.Reboots[i].completion[j])
		}
	}
	output.selected = slices.Clone(input.selected)
	for i := range output.selected {
		selected := &output.selected[i]
		host, err := copystructure.Copy(selected.host)
		if err != nil {
			return activationStageReceipt{}, err
		}
		device, err := copystructure.Copy(selected.device)
		if err != nil {
			return activationStageReceipt{}, err
		}
		adapter, err := copystructure.Copy(selected.adapter)
		if err != nil {
			return activationStageReceipt{}, err
		}
		selected.host = host.(inventory.Device)
		selected.device = device.(inventory.Device)
		selected.adapter = adapter.(inventory.Adapter)
		selected.item.device.SelectorLabels = maps.Clone(selected.item.device.SelectorLabels)
		if selected.item.target.SSH != nil {
			access := *selected.item.target.SSH
			selected.item.target.SSH = &access
		}
		if selected.hostTarget.SSH != nil {
			access := *selected.hostTarget.SSH
			selected.hostTarget.SSH = &access
		}
		selected.dpuSet = selected.dpuSet.DeepCopy()
	}
	return output, nil
}
func copyActivationNodeProgress(input activationNodeProgress) activationNodeProgress {
	input.sets, input.dpus, input.versions = maps.Clone(input.sets), maps.Clone(input.dpus), maps.Clone(input.versions)
	return input
}
func copyActivationProgress(input map[string]activationNodeProgress) map[string]activationNodeProgress {
	if input == nil {
		return nil
	}
	output := make(map[string]activationNodeProgress, len(input))
	for node, progress := range input {
		output[node] = copyActivationNodeProgress(progress)
	}
	return output
}
