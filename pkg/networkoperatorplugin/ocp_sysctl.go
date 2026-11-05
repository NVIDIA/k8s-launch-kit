// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ocpSysctlAllowlistPatchFilename = "04-cni-sysctl-allowlist-patch.yaml"

func isOCPSysctlAllowlistPatch(obj *unstructured.Unstructured) bool {
	return obj.GetAPIVersion() == "v1" && obj.GetKind() == "ConfigMap" &&
		obj.GetNamespace() == "openshift-multus" && obj.GetName() == "cni-sysctl-allowlist"
}

func sysctlAllowlistPatchEntries(desired *unstructured.Unstructured) (string, error) {
	required, found, err := unstructured.NestedString(desired.Object, "data", "allowlist.conf")
	if err != nil {
		return "", fmt.Errorf("read sysctl allowlist patch: %w", err)
	}
	if !found || strings.TrimSpace(required) == "" {
		return "", fmt.Errorf("sysctl allowlist patch requires non-empty data[allowlist.conf]")
	}
	return required, nil
}

// mergeOCPSysctlAllowlist treats the generated ConfigMap as an additive patch.
// Neither the artifact's metadata nor unrelated data keys claim platform ownership.
func mergeOCPSysctlAllowlist(current, desired *unstructured.Unstructured) error {
	required, err := sysctlAllowlistPatchEntries(desired)
	if err != nil {
		return err
	}
	existing, _, err := unstructured.NestedString(current.Object, "data", "allowlist.conf")
	if err != nil {
		return fmt.Errorf("read existing sysctl allowlist: %w", err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		seen[strings.TrimSpace(line)] = true
	}
	merged := existing
	for _, line := range strings.Split(required, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || seen[entry] {
			continue
		}
		if merged != "" && !strings.HasSuffix(merged, "\n") {
			merged += "\n"
		}
		merged += entry + "\n"
		seen[entry] = true
	}
	if merged == existing {
		return nil
	}
	return unstructured.SetNestedField(current.Object, merged, "data", "allowlist.conf")
}

func ensureOCPSysctlAllowlist(ctx context.Context, c client.Client, desired *unstructured.Unstructured, dryRun bool) error {
	key := client.ObjectKeyFromObject(desired)
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(desired.GroupVersionKind())
		// Require the platform ConfigMap; absence or failed inspection is an error.
		if err := c.Get(ctx, key, current); err != nil {
			return err
		}
		before := current.DeepCopy()
		if err := mergeOCPSysctlAllowlist(current, desired); err != nil {
			return err
		}
		if reflect.DeepEqual(before.Object, current.Object) {
			return nil
		}
		opts := []client.PatchOption{}
		if dryRun {
			opts = append(opts, client.DryRunAll)
		}
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		if err := c.Patch(ctx, current, patch, opts...); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("patch OpenShift sysctl allowlist %s: %w", key, err)
	}
	if changed {
		ui.FromContext(ctx).Info("Configured OpenShift Multus sysctl allowlist (dry-run: %t)", dryRun)
	}
	return nil
}
