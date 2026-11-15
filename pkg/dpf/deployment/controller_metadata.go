// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"maps"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Dynamic protection is limited to the exact retained software object and the
// exact DPUSet names compiled from the selected-device selectors. A token is
// not evidence of a live DPUSet UID; activation separately verifies that graph.
type softwareIdentity struct{ namespace, name string }
type controllerMetadataPolicy map[softwareIdentity]map[string]struct{}

func (p controllerMetadataPolicy) clone() controllerMetadataPolicy {
	result := make(controllerMetadataPolicy, len(p))
	for key, tokens := range p {
		result[key] = maps.Clone(tokens)
	}
	return result
}

func controllerMetadataForPlan(plan *generation.Plan) (controllerMetadataPolicy, error) {
	if plan == nil {
		return nil, fmt.Errorf("validated retained plan is required for controller metadata")
	}
	intent := plan.Intent()
	var deployment, software *unstructured.Unstructured
	for _, view := range plan.Stages() {
		stage := view.Stage()
		if stage.ID != generation.StageDeployment && stage.ID != generation.StageWorkload {
			continue
		}
		if stage.Destination != bundle.Management || stage.Namespace != intent.Namespace {
			return nil, fmt.Errorf("controller metadata stage differs from retained namespace or destination")
		}
		for _, document := range view.Documents() {
			object := document.ObjectCopy()
			if object == nil {
				return nil, fmt.Errorf("controller metadata document is missing")
			}
			switch {
			case stage.ID == generation.StageDeployment && object.GetKind() == "DPUDeployment":
				if deployment != nil {
					return nil, fmt.Errorf("retained DPUDeployment is ambiguous")
				}
				deployment = object
			case stage.ID == generation.StageWorkload && object.GetKind() == "BlueFieldSoftware":
				if software != nil {
					return nil, fmt.Errorf("retained BlueFieldSoftware is ambiguous")
				}
				software = object
			}
		}
	}
	if deployment == nil || deployment.GetNamespace() != intent.Namespace {
		return nil, fmt.Errorf("retained DPUDeployment is missing or outside the selected namespace")
	}
	return compileControllerMetadata(deployment, software, intent.Devices)
}

func compileControllerMetadata(deployment, software *unstructured.Unstructured, devices []generation.ResolvedDevice) (controllerMetadataPolicy, error) {
	sets, err := activationSetIntents(deployment, devices)
	if err != nil {
		return nil, err
	}
	if software == nil || software.GetAPIVersion() != blueFieldSoftwareAPIVersion || software.GetKind() != "BlueFieldSoftware" || software.GetNamespace() != deployment.GetNamespace() {
		return nil, fmt.Errorf("retained BlueFieldSoftware identity or namespace differs from deployment")
	}
	tokens := make(map[string]struct{}, len(sets))
	for _, set := range sets {
		name, _, err := activationSetReferences(set)
		if err != nil || name != software.GetName() {
			return nil, fmt.Errorf("retained deployment does not reference its exact BlueFieldSoftware")
		}
		tokens["provisioning.dpu.nvidia.com/bfs-dpuset-"+set.GetName()] = struct{}{}
	}
	return controllerMetadataPolicy{softwareIdentity{namespace: software.GetNamespace(), name: software.GetName()}: tokens}, nil
}

func validateControllerMetadata(object *unstructured.Unstructured, policy controllerMetadataPolicy) error {
	allowed := map[string]map[string]bool{
		"DPFOperatorConfig": {"dpu.nvidia.com/dpfoperatorconfig": true},
		"DPUCluster": {
			"provisioning.dpu.nvidia.com/cluster-manager-clean-up":          true,
			"provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up": true,
		},
		"DPUDeployment": {"dpu.nvidia.com/dpudeployment": true},
		"BlueFieldSoftware": {
			"dpu.nvidia.com/dpudeployment":                             true,
			"provisioning.dpu.nvidia.com/bluefieldsoftware-protection": true,
		},
		"DPUFlavor": {"dpu.nvidia.com/dpudeployment": true},
		"DPUServiceTemplate": {
			"dpu.nvidia.com/dpudeployment":      true,
			"dpu.nvidia.com/dpuservicetemplate": true,
		},
		"DPUServiceConfiguration": {"dpu.nvidia.com/dpudeployment": true},
		"DPUServiceInterface":     {"svc.dpu.nvidia.com/dpuserviceinterface": true},
		"DPUServiceIPAM":          {"svc.dpu.nvidia.com/dpuserviceipam": true},
	}
	seen := make(map[string]bool, len(object.GetFinalizers()))
	for _, finalizer := range object.GetFinalizers() {
		recognized := allowed[object.GetKind()][finalizer]
		if object.GetKind() == "BlueFieldSoftware" {
			_, fromPlan := policy[softwareIdentity{namespace: object.GetNamespace(), name: object.GetName()}][finalizer]
			recognized = recognized || fromPlan
		}
		if !recognized || seen[finalizer] {
			return fmt.Errorf("current object has an unrecognized finalizer")
		}
		seen[finalizer] = true
	}
	return nil
}
