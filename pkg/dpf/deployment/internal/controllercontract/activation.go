// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

const activationParentLabel = "svc.dpu.nvidia.com/owned-by-dpudeployment"

func (r *Rules) ActivationSetIntents(deployment *unstructured.Unstructured, selected []generation.ResolvedDevice) (map[string]*unstructured.Unstructured, error) {
	if !r.valid() {
		return nil, fmt.Errorf("selected controller rules are required")
	}
	if deployment == nil || deployment.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || deployment.GetKind() != "DPUDeployment" ||
		len(validation.IsDNS1123Subdomain(deployment.GetName())) != 0 || len(validation.IsDNS1123Label(deployment.GetNamespace())) != 0 || len(selected) == 0 {
		return nil, fmt.Errorf("retained DPUDeployment and selected device identities are required")
	}
	software, softwareFound, softwareErr := unstructured.NestedString(deployment.Object, "spec", "dpus", "blueFieldSoftware")
	flavor, flavorFound, flavorErr := unstructured.NestedString(deployment.Object, "spec", "dpus", "flavor")
	if softwareErr != nil || !softwareFound || len(validation.IsDNS1123Subdomain(software)) != 0 || flavorErr != nil || !flavorFound || len(validation.IsDNS1123Subdomain(flavor)) != 0 ||
		nestedFieldPresent(deployment.Object, "spec", "dpus", "bfb") || nestedFieldPresent(deployment.Object, "spec", "dpus", "flavorTemplate") {
		return nil, fmt.Errorf("native activation requires explicit BlueFieldSoftware and flavor names, without BFB or flavor templates")
	}
	settings, found, err := unstructured.NestedSlice(deployment.Object, "spec", "dpus", "dpuSets")
	if err != nil || !found || len(settings) != len(selected) {
		return nil, fmt.Errorf("retained DPUDeployment does not contain one DPUSet selector per selected DPU")
	}
	sets := make(map[string]*unstructured.Unstructured, len(selected))
	seenSuffix := map[string]bool{}
	for _, settingRaw := range settings {
		setting, ok := settingRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("retained DPUSet selector is malformed")
		}
		suffix, ok := setting["nameSuffix"].(string)
		if !ok || suffix == "" || seenSuffix[suffix] || len(validation.IsDNS1123Subdomain(deployment.GetName()+"-"+suffix)) != 0 {
			return nil, fmt.Errorf("retained DPUSet name suffix is missing, invalid or duplicated")
		}
		seenSuffix[suffix] = true
		selector, found, nestedErr := unstructured.NestedStringMap(setting, "dpuDeviceSelector", "matchLabels")
		if nestedErr != nil || !found || len(selector) == 0 {
			return nil, fmt.Errorf("retained DPUSet device selector is missing or malformed")
		}
		matches := 0
		serial := ""
		for _, item := range selected {
			labels := item.SelectorLabels
			if len(labels) > 0 && reflect.DeepEqual(selector, labels) {
				matches++
				serial = strings.ToLower(item.Serial)
			}
		}
		if matches != 1 || sets[serial] != nil {
			return nil, fmt.Errorf("retained DPUSet selector does not uniquely match one selected device")
		}
		clusterSelector, found, nestedErr := unstructured.NestedStringMap(setting, "dpuClusterSelector")
		if nestedErr != nil || !found || len(clusterSelector) == 0 {
			return nil, fmt.Errorf("retained DPUSet tenant selector is missing or malformed")
		}
		set := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1",
			"kind":       "DPUSet",
			"metadata": map[string]any{
				"name":      deployment.GetName() + "-" + suffix,
				"namespace": deployment.GetNamespace(),
				"labels":    map[string]any{activationParentLabel: deployment.GetNamespace() + "_" + deployment.GetName()},
			},
			"spec": map[string]any{
				"dpuDeviceSelector": map[string]any{"matchLabels": stringMapJSON(selector)},
				"dpuTemplate": map[string]any{
					"spec": map[string]any{
						"blueFieldSoftware": map[string]any{"name": software},
						"dpuFlavor":         flavor,
						"cluster": map[string]any{
							"selector": map[string]any{"matchLabels": stringMapJSON(clusterSelector)},
						},
					},
				},
			},
		}}
		sets[serial] = set
	}
	if len(sets) != len(selected) {
		return nil, fmt.Errorf("retained DPUSet selectors do not cover every selected device")
	}
	return sets, nil
}

func (r *Rules) GeneratedDPUEName(nodeName, deviceName string) string {
	if !r.valid() || len(deviceName) > 62 {
		return ""
	}
	const maxLength = 63
	maxPrefix := maxLength - len(deviceName) - 1
	if len(nodeName) > maxPrefix {
		nodeName = nodeName[:maxPrefix]
	}
	return nodeName + "-" + deviceName
}

func (r *Rules) ActivationSetReferences(set *unstructured.Unstructured) (string, string, error) {
	if !r.valid() {
		return "", "", fmt.Errorf("selected controller rules are required")
	}
	if set == nil {
		return "", "", fmt.Errorf("selected DPUSet intent is missing")
	}
	software, found, err := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "blueFieldSoftware")
	flavor, flavorFound, flavorErr := unstructured.NestedString(set.Object, "spec", "dpuTemplate", "spec", "dpuFlavor")
	if err != nil || !found || len(software) != 1 || len(validation.IsDNS1123Subdomain(software["name"])) != 0 || flavorErr != nil || !flavorFound || len(validation.IsDNS1123Subdomain(flavor)) != 0 ||
		nestedFieldPresent(set.Object, "spec", "dpuTemplate", "spec", "bfb") || nestedFieldPresent(set.Object, "spec", "dpuTemplate", "spec", "dpuFlavorTemplate") {
		return "", "", fmt.Errorf("selected DPUSet consumed software or flavor reference is missing or unsupported")
	}
	return software["name"], flavor, nil
}

func nestedFieldPresent(object map[string]any, fields ...string) bool {
	_, found, err := unstructured.NestedFieldNoCopy(object, fields...)
	return found || err != nil
}

func stringMapJSON(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for k, v := range values {
		result[k] = v
	}
	return result
}
