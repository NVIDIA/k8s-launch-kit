// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type deploymentObjects struct {
	ocpOwner        string // Bound to the selected OpenShift root before any deployment writes.
	ncp             *unstructured.Unstructured
	nnps            []*unstructured.Unstructured
	others          []*unstructured.Unstructured
	operatorConfigs []*unstructured.Unstructured
	// operatorInstall holds the opt-in OLM install artifacts (Namespace,
	// OperatorGroup, Subscription). Ordinary deploy does not apply them: operator
	// installation is manual until readiness-gated management lands.
	operatorInstall []*unstructured.Unstructured
}

func prepareDeploymentObjects(artifacts *bundle.Bundle, ocp bool) (deploymentObjects, error) {
	if artifacts == nil {
		return deploymentObjects{}, fmt.Errorf("deployment artifact bundle is nil")
	}
	var selected deploymentObjects
	for _, doc := range artifacts.Documents() {
		if doc.Role() != bundle.Deployment {
			continue
		}
		obj := doc.ObjectCopy()
		if isOLMInstallTemplate(filepath.Base(doc.Source().File)) {
			selected.operatorInstall = append(selected.operatorInstall, obj)
			continue
		}
		if doc.Source().File == ocpSysctlAllowlistPatchFilename && !isOCPSysctlAllowlistPatch(obj) {
			return deploymentObjects{}, fmt.Errorf("%s: expected ConfigMap openshift-multus/cni-sysctl-allowlist", doc.Source().File)
		}
		if isOCPSysctlAllowlistPatch(obj) {
			if _, err := sysctlAllowlistPatchEntries(obj); err != nil {
				return deploymentObjects{}, fmt.Errorf("%s: %w", doc.Source().File, err)
			}
			if !ocp {
				return deploymentObjects{}, fmt.Errorf("%s: OpenShift sysctl allowlist patch requires flavor ocp", doc.Source().File)
			}
		}

		switch obj.GetKind() {
		case "NicClusterPolicy":
			if selected.ncp != nil {
				return deploymentObjects{}, fmt.Errorf("multiple NicClusterPolicy manifests found; only one is allowed")
			}
			selected.ncp = obj
		case "NicNodePolicy":
			selected.nnps = append(selected.nnps, obj)
		default:
			if ocp && isOCPOperatorConfig(obj) {
				selected.operatorConfigs = append(selected.operatorConfigs, obj)
			} else {
				selected.others = append(selected.others, obj)
			}
		}
	}
	return selected, nil
}
