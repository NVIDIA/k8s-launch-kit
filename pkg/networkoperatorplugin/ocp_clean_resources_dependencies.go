// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// checkOCPCleanDependencies must also run for --skip-wait: that option waives
// reconciliation evidence, not protection of omitted resources and shared users.
// The coordinator can run this read-only guard before displaying confirmation.
func checkOCPCleanDependencies(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, refs []cleanObjectRef) error {
	var root *unstructured.Unstructured
	selected := map[string]bool{}
	for _, ref := range refs {
		selected[ocpCleanIdentity(ref.GVK, ref.Namespace, ref.Name)] = true
		if ref.GVK.Group == "mellanox.com" && ref.GVK.Kind == "NicClusterPolicy" {
			var err error
			root, err = ocpDesired(ref)
			if err != nil {
				return err
			}
		}
	}
	if err := checkOCPPoolConsumers(ctx, c, cfg, refs); err != nil {
		return err
	}
	if root == nil {
		return nil
	}
	names := ocpCleanNamespaces(cfg)
	owner := root.GetAnnotations()[ocpCleanOwnerAnnotation]
	configured := func(field string) bool {
		value, found, _ := unstructured.NestedFieldNoCopy(root.Object, "spec", field)
		return found && value != nil
	}
	for _, entry := range []struct {
		group, version, kind, namespace string
		dependent                       bool
	}{
		{"mellanox.com", "v1alpha1", "NicNodePolicy", "", true},
		{"mellanox.com", "v1alpha1", "HostDeviceNetwork", "", false},
		{"mellanox.com", "v1alpha1", "IPoIBNetwork", "", configured("secondaryNetwork")},
		{"mellanox.com", "v1alpha1", "MacvlanNetwork", "", false},
		{cleanNICGroup, "v1alpha1", "NicConfigurationTemplate", names.operator, configured("nicConfigurationOperator")},
		{cleanNICGroup, "v1alpha1", "NicFirmwareTemplate", names.operator, configured("nicConfigurationOperator")},
		{cleanNICGroup, "v1alpha1", "NicFirmwareSource", names.operator, configured("nicConfigurationOperator")},
		{cleanNICGroup, "v1alpha1", "NicInterfaceNameTemplate", names.operator, configured("nicConfigurationOperator")},
		{"nv-ipam.nvidia.com", "v1alpha1", "IPPool", names.operator, configured("nvIpam")},
		{"nv-ipam.nvidia.com", "v1alpha1", "CIDRPool", names.operator, configured("nvIpam")},
		{cleanSriovGroup, "v1", "SriovNetworkNodePolicy", names.sriov, false},
		{cleanSriovGroup, "v1", "SriovNetworkPoolConfig", names.sriov, false},
		{cleanSriovGroup, "v1", "SriovNetwork", names.sriov, false},
		{cleanSriovGroup, "v1", "SriovIBNetwork", names.sriov, false},
	} {
		gvk := schema.GroupVersionKind{Group: entry.group, Version: entry.version, Kind: entry.kind}
		peers, err := listCleanNetworkObjects(ctx, c, gvk, entry.namespace)
		if err != nil {
			return fmt.Errorf("inspect dependencies before deleting NicClusterPolicy %s: %w", root.GetName(), err)
		}
		for i := range peers {
			peer := &peers[i]
			if selected[ocpCleanIdentity(gvk, peer.GetNamespace(), peer.GetName())] {
				continue
			}
			sameRoot := owner != "" && peer.GetAnnotations()[ocpCleanOwnerAnnotation] == owner
			if entry.dependent || sameRoot {
				return fmt.Errorf("refuse deletion of NicClusterPolicy %s: omitted %s %s/%s still depends on its controllers or belongs to the same deployment; regenerate a complete bundle or retain the shared root", root.GetName(), entry.kind, peer.GetNamespace(), peer.GetName())
			}
		}
	}
	return nil
}

func ocpCleanIdentity(gvk schema.GroupVersionKind, namespace, name string) string {
	return gvk.Group + "/" + gvk.Kind + "/" + namespace + "/" + name
}
