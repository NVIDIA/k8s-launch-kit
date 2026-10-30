// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Preserve the standard interface allowlist along with ignoreARP's global and
// interface entries. This is shared platform configuration, not an l8k-owned CR.
const ocpIgnoreARPAllowlist = `^net.ipv4.conf.all.arp_ignore$
^net.ipv4.conf.all.arp_announce$
^net.ipv4.conf.all.rp_filter$
^net.ipv4.conf.IFNAME.accept_redirects$
^net.ipv4.conf.IFNAME.accept_source_route$
^net.ipv4.conf.IFNAME.arp_accept$
^net.ipv4.conf.IFNAME.arp_announce$
^net.ipv4.conf.IFNAME.arp_ignore$
^net.ipv4.conf.IFNAME.arp_notify$
^net.ipv4.conf.IFNAME.disable_policy$
^net.ipv4.conf.IFNAME.rp_filter$
^net.ipv4.conf.IFNAME.secure_redirects$
^net.ipv4.conf.IFNAME.send_redirects$
^net.ipv6.conf.IFNAME.accept_ra$
^net.ipv6.conf.IFNAME.accept_redirects$
^net.ipv6.conf.IFNAME.accept_source_route$
^net.ipv6.conf.IFNAME.arp_accept$
^net.ipv6.conf.IFNAME.arp_notify$
^net.ipv6.neigh.IFNAME.base_reachable_time_ms$
^net.ipv6.neigh.IFNAME.retrans_time_ms$`

// Inspect desired network artifacts rather than a possibly stale source config:
// standalone deploy must prepare the allowlist for exactly what it will apply.
func networksNeedARPAllowlist(docs []*unstructured.Unstructured) (bool, error) {
	needed := false
	for _, doc := range docs {
		switch doc.GroupVersionKind().Group + "/" + doc.GetKind() {
		case "sriovnetwork.openshift.io/SriovNetwork", "sriovnetwork.openshift.io/SriovIBNetwork",
			"mellanox.com/HostDeviceNetwork", "mellanox.com/MacvlanNetwork", "mellanox.com/IPoIBNetwork":
		default:
			continue
		}
		raw, _, err := unstructured.NestedString(doc.Object, "spec", "metaPlugins")
		if err != nil {
			return false, fmt.Errorf("read %s %s metaPlugins: %w", doc.GetKind(), doc.GetName(), err)
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var plugins []struct {
			Type   string            `json:"type"`
			Sysctl map[string]string `json:"sysctl"`
		}
		// Network CRs contain a comma-separated list of plugins, without brackets.
		if err := json.Unmarshal([]byte("["+raw+"]"), &plugins); err != nil {
			return false, fmt.Errorf("decode %s %s metaPlugins: %w", doc.GetKind(), doc.GetName(), err)
		}
		for _, plugin := range plugins {
			if plugin.Type != "tuning" {
				continue
			}
			for _, scope := range []string{"all", "IFNAME"} {
				for _, name := range []string{"arp_ignore", "arp_announce", "rp_filter"} {
					if _, ok := plugin.Sysctl["net.ipv4.conf."+scope+"."+name]; ok {
						needed = true
					}
				}
			}
		}
	}
	return needed, nil
}

func ensureOCPSysctlAllowlist(ctx context.Context, c client.Client, docs []*unstructured.Unstructured, dryRun bool) error {
	needed, err := networksNeedARPAllowlist(docs)
	if err != nil || !needed {
		return err
	}
	key := types.NamespacedName{Namespace: "openshift-multus", Name: "cni-sysctl-allowlist"}
	changed := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		current := &corev1.ConfigMap{}
		// Require the platform ConfigMap; absence or failed inspection is an error.
		if err := c.Get(ctx, key, current); err != nil {
			return err
		}
		before := current.DeepCopy()
		existing := current.Data["allowlist.conf"]
		seen := map[string]bool{}
		for _, line := range strings.Split(existing, "\n") {
			seen[strings.TrimSpace(line)] = true
		}
		merged := existing
		for _, entry := range strings.Split(ocpIgnoreARPAllowlist, "\n") {
			if seen[entry] {
				continue
			}
			if merged != "" && !strings.HasSuffix(merged, "\n") {
				merged += "\n"
			}
			merged += entry + "\n"
		}
		if merged == existing {
			return nil
		}
		if current.Data == nil {
			current.Data = map[string]string{}
		}
		current.Data["allowlist.conf"] = merged
		opts := []client.PatchOption{}
		if dryRun {
			opts = append(opts, client.DryRunAll)
		}
		// resourceVersion prevents overwriting an administrator's concurrent edit.
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
