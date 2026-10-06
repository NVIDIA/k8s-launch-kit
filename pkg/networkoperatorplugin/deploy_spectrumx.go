// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"slices"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const spectrumXTemplateCRD = "nicconfigurationtemplates.configuration.net.nvidia.com"

// validateSpectrumXDeploymentAPI checks the bundle's requirements against the
// live CRD after NCP reconciliation has installed or upgraded it. Inspecting the
// manifests also protects standalone deploy without a resolved-config sidecar.
func validateSpectrumXDeploymentAPI(ctx context.Context, kubeClient client.Client, objects []*unstructured.Unstructured) error {
	var versions []string
	for _, obj := range objects {
		if obj == nil || obj.GetKind() != "NicConfigurationTemplate" || obj.GroupVersionKind().Group != "configuration.net.nvidia.com" {
			continue
		}
		ra, _, err := unstructured.NestedString(obj.Object, "spec", "template", "spectrumXOptimized", "version")
		if err != nil {
			return fmt.Errorf("inspect %s Spectrum-X version: %w", obj.GetName(), err)
		}
		_, hasPlatform, err := unstructured.NestedString(obj.Object, "spec", "template", "spectrumXOptimized", "platformType")
		if err != nil {
			return fmt.Errorf("inspect %s Spectrum-X platformType: %w", obj.GetName(), err)
		}
		if ra != "RA2.4" && !hasPlatform {
			continue
		}
		version := obj.GroupVersionKind().Version
		if !slices.Contains(versions, version) {
			versions = append(versions, version)
		}
	}
	if len(versions) == 0 {
		return nil
	}
	if kubeClient == nil {
		return fmt.Errorf("a Kubernetes client is required to verify the RA2.4 NIC Configuration Operator API")
	}
	crd := &apiextv1.CustomResourceDefinition{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: spectrumXTemplateCRD}, crd); err != nil {
		return fmt.Errorf("cannot verify RA2.4 support in CRD %s: %w", spectrumXTemplateCRD, err)
	}
	for _, requiredVersion := range versions {
		supported := false
		for _, version := range crd.Spec.Versions {
			if version.Name != requiredVersion || !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			field := *version.Schema.OpenAPIV3Schema
			for _, key := range []string{"spec", "template", "spectrumXOptimized", "platformType"} {
				field = field.Properties[key]
			}
			supported = field.Type == "string"
			break
		}
		if !supported {
			return fmt.Errorf("installed NIC Configuration Operator CRD %s does not serve %s with string field spec.template.spectrumXOptimized.platformType; RA2.4 requires a compatible operator implementation and matching CRDs", spectrumXTemplateCRD, requiredVersion)
		}
	}
	return nil
}
