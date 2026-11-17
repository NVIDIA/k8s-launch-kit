// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"k8s.io/apimachinery/pkg/types"
)

var errOperationHostChanged = errors.New("HOST_NETWORK_OPERATION_CHANGED: saved request differs")

func (o *deploymentOperation) bindingConflict(message string) error {
	o.failed = true
	return errors.New(message)
}

func (o *deploymentOperation) writable() error {
	if o.failed || o.object == nil {
		return fmt.Errorf("OPERATION_CHECKPOINT_UNCERTAIN: stop before another effect")
	}
	return nil
}
func (o *deploymentOperation) bindDeployment(ctx context.Context, uid types.UID) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	if uid == "" {
		return fmt.Errorf("OPERATION_PARENT_CHANGED: deployment UID required")
	}
	if o.state.DeploymentUID != "" && o.state.DeploymentUID != uid {
		return o.bindingConflict("OPERATION_PARENT_CHANGED: deployment binding differs")
	}
	if o.state.DeploymentUID == uid {
		return nil
	}
	o.state.DeploymentUID = uid
	return o.save(ctx)
}
func (o *deploymentOperation) validateProgress(p operationProgress) error {
	if o.state.DeploymentUID == "" || p.NodeName == "" || p.MaintenanceUID == "" || len(p.Sets) == 0 || len(p.DPUs) == 0 || len(p.Versions) == 0 {
		return fmt.Errorf("OPERATION_PROGRESS_INVALID: complete selected bindings required")
	}
	node, known := o.state.Children["DPUNode/"+p.NodeName]
	if !known || node.UID == "" {
		return fmt.Errorf("OPERATION_PROGRESS_INVALID: node unbound")
	}
	for kind, objects := range map[string]map[string]types.UID{"DPUSet": p.Sets, "DPU": p.DPUs} {
		for name, uid := range objects {
			child, found := o.state.Children[kind+"/"+name]
			if !found || uid == "" || child.UID != uid {
				return fmt.Errorf("OPERATION_PROGRESS_INVALID: child binding changed")
			}
		}
	}
	for name, uid := range p.Versions {
		if name == "" || uid == "" {
			return fmt.Errorf("OPERATION_PROGRESS_INVALID: version binding incomplete")
		}
	}
	return nil
}
func sameProgressCohort(a, b operationProgress) bool {
	return a.NodeName == b.NodeName && a.MaintenanceUID == b.MaintenanceUID && reflect.DeepEqual(a.Sets, b.Sets) && reflect.DeepEqual(a.DPUs, b.DPUs) && reflect.DeepEqual(a.Versions, b.Versions)
}
func (o *deploymentOperation) recordHoldIntent(ctx context.Context, proof activationNodeProgress) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	p := savedProgress(proof)
	if err := o.validateProgress(p); err != nil {
		return err
	}
	if p.ReleasedVersion == "" || p.ResetAcknowledged {
		return fmt.Errorf("OPERATION_HOLD_INVALID: pending hold version required")
	}
	if _, confirmed := o.state.Progress[p.NodeName]; confirmed {
		return fmt.Errorf("OPERATION_HOLD_INVALID: hold already confirmed")
	}
	if prior, known := o.state.HoldIntents[p.NodeName]; known && !sameProgressCohort(prior, p) {
		return o.bindingConflict("OPERATION_HOLD_INVALID: selected identities changed")
	}
	o.state.HoldIntents[p.NodeName] = p
	return o.save(ctx)
}
func (o *deploymentOperation) confirmHoldReleased(ctx context.Context, proof activationNodeProgress) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	p := savedProgress(proof)
	if err := o.validateProgress(p); err != nil {
		return err
	}
	intent, known := o.state.HoldIntents[p.NodeName]
	if !known || !sameProgressCohort(intent, p) || p.ReleasedVersion == "" || p.ResetAcknowledged {
		return fmt.Errorf("OPERATION_HOLD_INVALID: exact durable intent and released version required")
	}
	if prior, known := o.state.Progress[p.NodeName]; known && !reflect.DeepEqual(prior, p) {
		return o.bindingConflict("OPERATION_HOLD_INVALID: confirmed history changed")
	}
	o.state.Progress[p.NodeName] = p
	return o.save(ctx)
}

// recordHostIntent returns whether the original transaction must be inspected.
// An intent without a result is still known and never permits configuration.
func (o *deploymentOperation) recordHostIntent(ctx context.Context, serial string, request sshprobe.HostNetworkRequest) (bool, error) {
	if o == nil {
		return false, nil
	}
	if err := o.writable(); err != nil {
		return false, err
	}
	if o.state.DeploymentUID == "" || serial == "" || request.Identity.AdapterSerial != serial || request.Identity.SystemUUID == "" || request.Identity.InterfaceName == "" || request.HostAddressPrefix == "" || request.Gateway == "" || request.MTU < sshprobe.MinimumHostWorkloadMTU || !validSHA256Digest(request.LeaseBinding) {
		return false, fmt.Errorf("HOST_NETWORK_OPERATION_CHANGED: incomplete host request")
	}
	enrolled := false
	for _, device := range o.state.Enrollment.Devices {
		if device.Serial == serial {
			enrolled = true
		}
	}
	if !enrolled {
		return false, fmt.Errorf("HOST_NETWORK_OPERATION_CHANGED: host outside enrolled cohort")
	}
	if prior, known := o.state.Hosts[serial]; known {
		if !sameOperationRequest(prior.Request, request) {
			o.failed = true
			return true, errOperationHostChanged
		}
		return true, nil
	}
	o.state.Hosts[serial] = operationHost{Request: request}
	return false, o.save(ctx)
}
func (o *deploymentOperation) confirmHostNetwork(ctx context.Context, serial string, request sshprobe.HostNetworkRequest, result HostWorkloadNetworkResult) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	prior, known := o.state.Hosts[serial]
	receipt := result.Configuration
	if !known || !sameOperationRequest(prior.Request, request) || result.DPUSerial != serial || result.HostInputRef == "" || result.Interface != request.Identity.InterfaceName || result.HostAddressPrefix != request.HostAddressPrefix || result.Gateway != request.Gateway || result.MTU != request.MTU || !result.LeaseReverified || receipt.Blocking || receipt.State != "complete" && receipt.State != "verified-external" || !receipt.RuntimeVerified || !receipt.FileVerified {
		return fmt.Errorf("HOST_NETWORK_RECEIPT_INCOMPLETE: exact definitive transaction required")
	}
	// Preserve the first definitive transaction's provenance across inspection.
	if prior.Result != nil {
		return nil
	}
	prior.Result = &result
	o.state.Hosts[serial] = prior
	return o.save(ctx)
}
