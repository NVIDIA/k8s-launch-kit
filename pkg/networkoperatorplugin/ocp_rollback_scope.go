// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ocpConfigurationRestoreTarget struct {
	Object   *unstructured.Unstructured
	Optional bool
}

// This finite set also finds a journal left by a previous version of the same
// bundle when it no longer renders an optional shared configuration artifact.
func ocpConfigurationSingletons(cfg *config.LaunchKitConfig) []*unstructured.Unstructured {
	names := ocpCleanNamespaces(cfg)
	specs := []struct{ api, kind, namespace, name string }{
		{"sriovnetwork.openshift.io/v1", "SriovOperatorConfig", names.sriov, "default"},
		{"maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", names.maintenance, "default"},
		{"nfd.openshift.io/v1", "NodeFeatureDiscovery", names.nfd, names.nfdName},
		{"v1", "ConfigMap", "openshift-multus", "cni-sysctl-allowlist"},
	}
	objects := make([]*unstructured.Unstructured, 0, len(specs))
	for _, spec := range specs {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(spec.api)
		obj.SetKind(spec.kind)
		obj.SetNamespace(spec.namespace)
		obj.SetName(spec.name)
		objects = append(objects, obj)
	}
	return objects
}
func ocpOptionalConfigurationAbsent(err error) bool { return err != nil && meta.IsNoMatchError(err) }
func ocpConfigurationHasCohort(obj *unstructured.Unstructured, cohort string) bool {
	raw := obj.GetAnnotations()[ocpRollbackAnnotation]
	var header struct {
		Cohorts []string `json:"cohorts"`
	}
	if err := json.Unmarshal([]byte(raw), &header); err != nil {
		return strings.Contains(raw, cohort)
	}
	return containsOCPString(header.Cohorts, cohort)
}

// A changed custom namespace/name cannot silently orphan the previous journal.
// Inspect metadata on just the three shared CR kinds, never broad CR payloads or
// operator namespaces. A claim at another identity requires the prior bundle.
func checkOCPConfigurationMapping(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, cohort string) error {
	if c == nil || cfg == nil || cohort == "" {
		return fmt.Errorf("OpenShift configuration requires client, config and cohort")
	}
	for _, expected := range ocpConfigurationSingletons(cfg) {
		if expected.GetKind() == "ConfigMap" {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: expected.GroupVersionKind().Group, Version: expected.GroupVersionKind().Version, Kind: expected.GetKind() + "List"})
		if err := c.List(ctx, list); ocpOptionalConfigurationAbsent(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect prior OpenShift %s configuration claims: %w", expected.GetKind(), err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if !ocpConfigurationHasCohort(item, cohort) {
				continue
			}
			if _, err := readOCPConfigurationJournal(item); err != nil {
				return err
			}
			if client.ObjectKeyFromObject(item) != client.ObjectKeyFromObject(expected) {
				return fmt.Errorf("OpenShift %s %s still has this deployment's configuration claim; use its prior retained bundle to clean it before changing the shared configuration namespace/name", item.GetKind(), client.ObjectKeyFromObject(item))
			}
		}
	}
	return nil
}
