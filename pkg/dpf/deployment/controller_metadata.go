// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func controllerMetadataForPlan(rules *controllercontract.Rules, plan *generation.Plan) (controllercontract.MetadataPolicy, error) {
	if plan == nil {
		return controllercontract.MetadataPolicy{}, fmt.Errorf("validated retained plan is required for controller metadata")
	}
	intent := plan.Intent()
	var deployment, software *unstructured.Unstructured
	for _, view := range plan.Stages() {
		stage := view.Stage()
		if stage.ID != generation.StageDeployment && stage.ID != generation.StageWorkload {
			continue
		}
		if stage.Destination != bundle.Management || stage.Namespace != intent.Namespace {
			return controllercontract.MetadataPolicy{}, fmt.Errorf("controller metadata stage differs from retained namespace or destination")
		}
		for _, document := range view.Documents() {
			object := document.ObjectCopy()
			if object == nil {
				return controllercontract.MetadataPolicy{}, fmt.Errorf("controller metadata document is missing")
			}
			switch {
			case stage.ID == generation.StageDeployment && object.GetKind() == "DPUDeployment":
				if deployment != nil {
					return controllercontract.MetadataPolicy{}, fmt.Errorf("retained DPUDeployment is ambiguous")
				}
				deployment = object
			case stage.ID == generation.StageWorkload && object.GetKind() == "BlueFieldSoftware":
				if software != nil {
					return controllercontract.MetadataPolicy{}, fmt.Errorf("retained BlueFieldSoftware is ambiguous")
				}
				software = object
			}
		}
	}
	if deployment == nil || deployment.GetNamespace() != intent.Namespace {
		return controllercontract.MetadataPolicy{}, fmt.Errorf("retained DPUDeployment is missing or outside the selected namespace")
	}
	return rules.CompileMetadata(deployment, software, intent.Devices)
}
