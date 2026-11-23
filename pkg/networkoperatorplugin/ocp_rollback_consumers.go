// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
	case "NodeFeatureDiscovery":
		reason, err := ocpRemainingNetworkingConsumer(ctx, c, false)
		if err != nil || reason != "" {
			return reason, err
		}
		return ocpRemainingSriovConsumer(ctx, c, "")
	case "MaintenanceOperatorConfig", "Subscription":
		return ocpRemainingNetworkingConsumer(ctx, c, true)
	case "ConfigMap":
		if isOCPSysctlAllowlistPatch(obj) {
			return ocpRemainingSysctlConsumer(ctx, c, obj, journal)
		}
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
func ocpRemainingSysctlConsumer(ctx context.Context, c client.Client, obj *unstructured.Unstructured, journal *ocpConfigurationJournal) (string, error) {
	current, _, err := unstructured.NestedString(obj.Object, "data", "allowlist.conf")
	if err != nil {
		return "", err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(current, "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var expressions []*regexp.Regexp
	for _, field := range journal.Fields {
		if strings.Join(field.Path, "/") != "data/allowlist.conf" {
			continue
		}
		for _, addition := range field.Added {
			pattern, ok := addition.(string)
			if !ok {
				return "", fmt.Errorf("invalid sysctl rollback addition")
			}
			if !present[pattern] || strings.HasPrefix(pattern, "#") {
				continue
			}
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("cannot establish consumers of recorded sysctl allowlist expression: %w", err)
			}
			expressions = append(expressions, expression)
		}
	}
	if len(expressions) == 0 {
		return "", nil
	}
	nads, err := listCleanNetworkObjects(ctx, c, cleanNADGVK, "")
	if err != nil {
		return "", err
	}
	for i := range nads {
		nad := &nads[i]
		raw, _, err := unstructured.NestedString(nad.Object, "spec", "config")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var configuration map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &configuration); err != nil {
			return "", fmt.Errorf("cannot inspect sysctl dependencies of NetworkAttachmentDefinition %s: %w", client.ObjectKeyFromObject(nad), err)
		}
		if ocpCNIUsesSysctl(configuration, expressions) {
			return fmt.Sprintf("remaining NetworkAttachmentDefinition %s uses a sysctl authorized by the recorded allowlist additions", client.ObjectKeyFromObject(nad)), nil
		}
	}
	return "", nil
}
func ocpCNIUsesSysctl(configuration map[string]interface{}, expressions []*regexp.Regexp) bool {
	if configuration["type"] == "tuning" {
		sysctls, _ := configuration["sysctl"].(map[string]interface{})
		for name := range sysctls {
			for _, expression := range expressions {
				if expression.MatchString(name) {
					return true
				}
			}
		}
	}
	plugins, _ := configuration["plugins"].([]interface{})
	for _, entry := range plugins {
		plugin, ok := entry.(map[string]interface{})
		if ok && ocpCNIUsesSysctl(plugin, expressions) {
			return true
		}
	}
	return false
}
