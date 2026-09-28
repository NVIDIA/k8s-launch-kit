// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type deploymentObjects struct {
	ncp             *unstructured.Unstructured
	nnps            []*unstructured.Unstructured
	others          []*unstructured.Unstructured
	operatorConfigs []*unstructured.Unstructured
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
