// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"reflect"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Validate complete intent, allowing only defaults authenticated by the actual
// served CRD schema. Neither l8k field ownership nor a desired subset explains
// extra live configuration: an old bundle must not delete newer managed intent.
func validateOCPCleanLiveOwnership(ctx context.Context, c client.Client, desired, live *unstructured.Unstructured) error {
	if err := validateOCPProvenance(desired, live, true); err != nil {
		return err
	}
	want, found, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil || !found || len(want) == 0 {
		return fmt.Errorf("retained desired spec is missing or invalid")
	}
	have, found, err := unstructured.NestedMap(live.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("live spec is missing or invalid")
	}
	if reflect.DeepEqual(have, want) {
		return nil
	}
	normalized, err := defaultOCPCleanDesired(ctx, c, desired)
	if err != nil {
		return fmt.Errorf("live spec differs from retained generated intent and defaults cannot be verified: %w", err)
	}
	want, found, err = unstructured.NestedMap(normalized.Object, "spec")
	if err != nil || !found || !reflect.DeepEqual(have, want) {
		return fmt.Errorf("live spec differs from retained generated intent after served-schema defaulting")
	}
	return nil
}

func defaultOCPCleanDesired(ctx context.Context, c client.Client, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if c == nil || desired == nil || !isOCPCleanKind(desired.GroupVersionKind()) {
		return nil, fmt.Errorf("schema defaulting requires a client and supported OpenShift cleanup kind")
	}
	// These are the actual plural names of the reviewed OCP profile CRDs.
	plurals := map[string]string{
		"NicClusterPolicy": "nicclusterpolicies", "NicNodePolicy": "nicnodepolicies",
		"HostDeviceNetwork": "hostdevicenetworks", "IPoIBNetwork": "ipoibnetworks", "MacvlanNetwork": "macvlannetworks",
		"SriovNetworkNodePolicy": "sriovnetworknodepolicies", "SriovNetworkPoolConfig": "sriovnetworkpoolconfigs",
		"SriovNetwork": "sriovnetworks", "SriovIBNetwork": "sriovibnetworks", "IPPool": "ippools",
		"NicConfigurationTemplate": "nicconfigurationtemplates", "NicInterfaceNameTemplate": "nicinterfacenametemplates",
	}
	gvk := desired.GroupVersionKind()
	plural := plurals[gvk.Kind]
	if plural == "" {
		return nil, fmt.Errorf("cleanup kind has no reviewed CRD identity")
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: plural + "." + gvk.Group}, crd); err != nil {
		return nil, fmt.Errorf("read %s served schema: %w", gvk.Kind, err)
	}
	wantScope := apiextensionsv1.ClusterScoped
	if desired.GetNamespace() != "" {
		wantScope = apiextensionsv1.NamespaceScoped
	}
	if crd.Spec.Group != gvk.Group || crd.Spec.Names.Kind != gvk.Kind || crd.Spec.Names.Plural != plural || crd.Spec.Scope != wantScope {
		return nil, fmt.Errorf("CRD identity or scope differs from cleanup target")
	}
	for _, version := range crd.Spec.Versions {
		if version.Name != gvk.Version || !version.Served {
			continue
		}
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			return nil, fmt.Errorf("served cleanup API has no OpenAPI schema")
		}
		internal := &apiextensions.JSONSchemaProps{}
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internal, nil); err != nil {
			return nil, fmt.Errorf("convert served cleanup schema: %w", err)
		}
		structural, err := structuralschema.NewStructural(internal)
		if err != nil {
			return nil, fmt.Errorf("build served cleanup structural schema: %w", err)
		}
		copy := desired.DeepCopy()
		defaulting.Default(copy.Object, structural)
		return copy, nil
	}
	return nil, fmt.Errorf("cleanup API version %s is not served by its CRD", gvk.Version)
}
