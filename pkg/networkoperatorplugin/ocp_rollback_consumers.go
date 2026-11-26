// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Claims protect current l8k cohorts. These read-only guards additionally retain
// effects when surviving manual or legacy intent has no cohort representation.
// They do not infer dependency from an empty platform-default object.
func ocpConfigurationConsumer(ctx context.Context, c client.Client, obj *unstructured.Unstructured, journal *ocpConfigurationJournal) (string, error) {
	if len(journal.Fields) == 0 && journal.Created == nil {
		return "", nil
	}
	switch obj.GetKind() {
	case "SriovOperatorConfig":
		return ocpRemainingSriovConsumer(ctx, c, obj.GetNamespace())
	case "MaintenanceOperatorConfig", "Subscription":
		return ocpRemainingNetworkingConsumer(ctx, c, true)
	}
	return "", nil
}
func ocpRemainingSriovConsumer(ctx context.Context, c client.Client, namespace string) (string, error) {
	objects, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetworkNodePolicy"}, namespace)
	if err != nil {
		return "", err
	}
	for i := range objects {
		obj := &objects[i]
		spec, _, err := unstructured.NestedMap(obj.Object, "spec")
		if err != nil {
			return "", err
		}
		if len(spec) == 0 {
			continue
		}
		// The platform default merely requests zero VFs and has no user NIC scope.
		if obj.GetName() == "default" {
			selector, _, err := unstructured.NestedMap(obj.Object, "spec", "nicSelector")
			if err != nil {
				return "", err
			}
			count, _, err := unstructured.NestedInt64(obj.Object, "spec", "numVfs")
			if err != nil {
				return "", err
			}
			if count == 0 && len(selector) == 0 {
				continue
			}
		}
		return fmt.Sprintf("remaining SriovNetworkNodePolicy %s may still require the shared operator settings", client.ObjectKeyFromObject(obj)), nil
	}
	return "", nil
}
func ocpRemainingNetworkingConsumer(ctx context.Context, c client.Client, maintenanceOnly bool) (string, error) {
	for _, kind := range []string{"NicClusterPolicy", "NicNodePolicy"} {
		objects, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind}, "")
		if err != nil {
			return "", err
		}
		for i := range objects {
			obj := &objects[i]
			spec, _, err := unstructured.NestedMap(obj.Object, "spec")
			if err != nil {
				return "", err
			}
			if len(spec) == 0 {
				continue
			}
			if maintenanceOnly && spec["ofedDriver"] == nil && spec["nicConfigurationOperator"] == nil {
				continue
			}
			return fmt.Sprintf("remaining %s %s may still require the shared operator settings", kind, client.ObjectKeyFromObject(obj)), nil
		}
	}
	if maintenanceOnly {
		requests, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: "maintenance.nvidia.com", Version: "v1alpha1", Kind: "NodeMaintenance"}, "")
		if err != nil {
			return "", err
		}
		for i := range requests {
			request := &requests[i]
			node, _, err := unstructured.NestedString(request.Object, "spec", "nodeName")
			if err != nil {
				return "", err
			}
			if node != "" {
				return fmt.Sprintf("remaining NodeMaintenance %s still requests maintenance for node %s", client.ObjectKeyFromObject(request), node), nil
			}
		}
	}
	return "", nil
}
