// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type softwareIdentity struct{ namespace, name string }

// MetadataPolicy is an immutable exact-token projection from retained inputs.
type MetadataPolicy struct {
	tokens map[softwareIdentity]map[string]struct{}
}

func (r *Rules) CompileMetadata(deployment, software *unstructured.Unstructured, devices []generation.ResolvedDevice) (MetadataPolicy, error) {
	sets, err := r.ActivationSetIntents(deployment, devices)
	if err != nil {
		return MetadataPolicy{}, err
	}
	if software == nil || software.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || software.GetKind() != "BlueFieldSoftware" || software.GetNamespace() != deployment.GetNamespace() {
		return MetadataPolicy{}, fmt.Errorf("retained BlueFieldSoftware identity or namespace differs from deployment")
	}
	tokens := make(map[string]struct{}, len(sets))
	for _, set := range sets {
		name, _, err := r.ActivationSetReferences(set)
		if err != nil || name != software.GetName() {
			return MetadataPolicy{}, fmt.Errorf("retained deployment does not reference its exact BlueFieldSoftware")
		}
		tokens["provisioning.dpu.nvidia.com/bfs-dpuset-"+set.GetName()] = struct{}{}
	}
	return MetadataPolicy{tokens: map[softwareIdentity]map[string]struct{}{softwareIdentity{namespace: software.GetNamespace(), name: software.GetName()}: tokens}}, nil
}

func (r *Rules) ValidateMetadata(object *unstructured.Unstructured, policy MetadataPolicy) error {
	if !r.valid() || object == nil {
		return fmt.Errorf("selected controller rules and object are required")
	}
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
			_, fromPlan := policy.tokens[softwareIdentity{namespace: object.GetNamespace(), name: object.GetName()}][finalizer]
			recognized = recognized || fromPlan
		}
		if !recognized || seen[finalizer] {
			return fmt.Errorf("current object has an unrecognized finalizer")
		}
		seen[finalizer] = true
	}
	return nil
}

func (r *Rules) LabelsMatch(desired, current *unstructured.Unstructured) bool {
	if !r.valid() || desired == nil || current == nil {
		return false
	}
	want, got := desired.GetLabels(), current.GetLabels()
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	if len(got) == len(want) {
		return true
	}
	if !controllerMayMarkDependency(current.GetKind()) || len(got) != len(want)+1 {
		return false
	}
	for key, value := range got {
		if _, expected := want[key]; expected {
			continue
		}
		const prefix = "svc.dpu.nvidia.com/consumed-by-dpudeployment-"
		suffix := strings.TrimPrefix(key, prefix)
		if suffix == key || len(suffix) != 10 || value != "" {
			return false
		}
		for _, character := range suffix {
			if !strings.ContainsRune("0123456789abcdef", character) {
				return false
			}
		}
	}
	return true
}

func controllerMayMarkDependency(kind string) bool {
	switch kind {
	case "BlueFieldSoftware", "DPUFlavor", "DPUServiceTemplate", "DPUServiceConfiguration":
		return true
	default:
		return false
	}
}
