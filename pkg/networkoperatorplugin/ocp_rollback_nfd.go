// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The retained worker rules determine the branch. Other APIs and historical
// creation claims do not describe whether this NFD contains additional rules.
func cleanOCPNFDConfiguration(ctx context.Context, c client.Client, manifest, planned *unstructured.Unstructured) ([]string, error) {
	if ctx == nil || c == nil || planned == nil || planned.GetKind() != "NodeFeatureDiscovery" {
		return nil, fmt.Errorf("NFD cleanup requires context, client and planned NFD")
	}
	key := client.ObjectKeyFromObject(planned)
	if manifest == nil {
		return []string{fmt.Sprintf("Retained OpenShift NodeFeatureDiscovery %s: no retained NFD rules are available for comparison", key)}, nil
	}
	if manifest.GroupVersionKind() != planned.GroupVersionKind() || client.ObjectKeyFromObject(manifest) != key {
		return nil, fmt.Errorf("NFD cleanup manifest differs from planned identity")
	}
	want, err := ocpNFDManifestRules(manifest)
	if err != nil {
		return nil, err
	}
	var deleted *unstructured.Unstructured
	var messages []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deleted, messages = nil, nil
		current := planned.DeepCopy()
		if err := c.Get(ctx, key, current); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		} else if err != nil {
			return err
		}
		if current.GetUID() == "" || current.GetUID() != planned.GetUID() || current.GetResourceVersion() == "" {
			return fmt.Errorf("NFD %s appeared or was replaced after planning", key)
		}
		have, err := ocpConfigurationView(current)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(want, have) {
			deleted = current.DeepCopy()
			uid, rv := current.GetUID(), current.GetResourceVersion()
			return client.IgnoreNotFound(c.Delete(ctx, current, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationBackground)))
		}
		if current.GetDeletionTimestamp() != nil {
			return fmt.Errorf("NFD %s is deleting while additional worker rules remain", key)
		}
		before := current.DeepCopy()
		if err := subtractOCPNFDPCIRules(have, want); err != nil {
			return err
		}
		if err := setOCPConfigurationView(current, have); err != nil {
			return err
		}
		messages = []string{fmt.Sprintf("Retained OpenShift NodeFeatureDiscovery %s: additional worker rules remain", key)}
		if reflect.DeepEqual(before.Object, current.Object) {
			return nil
		}
		if err := c.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		messages[0] += "; removed the retained manifest's PCI contribution"
		return nil
	})
	if err != nil {
		return messages, fmt.Errorf("clean NFD configuration %s: %w", key, err)
	}
	if deleted != nil {
		if err := waitForOCPCreatedConfigurationDeletion(ctx, c, deleted); err != nil {
			return messages, err
		}
	}
	return messages, nil
}

// Deployment contributes these two PCI fields. Label fields are a common
// projection for all PCI classes, so an altered or still-shared format survives.
func subtractOCPNFDPCIRules(have, want map[string]interface{}) error {
	path := []string{"nfd", "sources", "pci"}
	pci, found, err := unstructured.NestedMap(have, path...)
	if err != nil || !found {
		return err
	}
	selected, found, err := unstructured.NestedMap(want, path...)
	if err != nil || !found {
		return err
	}
	changed := false
	classes, classesPresent, err := unstructured.NestedSlice(pci, "deviceClassWhitelist")
	if err != nil {
		return err
	}
	requested, requestedPresent, err := unstructured.NestedSlice(selected, "deviceClassWhitelist")
	if err != nil {
		return err
	}
	if classesPresent && requestedPresent && len(classes) > 0 {
		remaining := []interface{}{}
		for _, entry := range classes {
			if !ocpListContains(requested, entry) {
				remaining = append(remaining, entry)
			}
		}
		if len(remaining) != len(classes) {
			changed = true
			if len(remaining) == 0 {
				delete(pci, "deviceClassWhitelist")
			} else {
				pci["deviceClassWhitelist"] = remaining
			}
		}
		classes = remaining
	}
	shared := !classesPresent || len(classes) > 0
	for field := range pci {
		shared = shared || field != "deviceClassWhitelist" && field != "deviceLabelFields"
	}
	if !shared {
		if fields, present := pci["deviceLabelFields"]; present && reflect.DeepEqual(fields, selected["deviceLabelFields"]) {
			if list, ok := fields.([]interface{}); ok && len(list) > 0 {
				delete(pci, "deviceLabelFields")
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	if len(pci) > 0 {
		return unstructured.SetNestedMap(have, pci, path...)
	}
	unstructured.RemoveNestedField(have, path...)
	sources, _, err := unstructured.NestedMap(have, "nfd", "sources")
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		unstructured.RemoveNestedField(have, "nfd", "sources")
	}
	return nil
}

func ocpNFDManifestRules(manifest *unstructured.Unstructured) (map[string]interface{}, error) {
	view, err := ocpConfigurationView(manifest)
	if err != nil {
		return nil, err
	}
	pci, found, err := unstructured.NestedMap(view, "nfd", "sources", "pci")
	if err != nil {
		return nil, err
	}
	hasRules := false
	for _, key := range []string{"deviceClassWhitelist", "deviceLabelFields"} {
		values, _, err := unstructured.NestedStringSlice(pci, key)
		if err != nil {
			return nil, fmt.Errorf("invalid retained NFD sources.pci.%s: %w", key, err)
		}
		hasRules = hasRules || len(values) > 0
	}
	if !found || !hasRules {
		return nil, fmt.Errorf("retained NFD worker rules must contain a non-empty PCI class or label-field list")
	}
	return view, nil
}
