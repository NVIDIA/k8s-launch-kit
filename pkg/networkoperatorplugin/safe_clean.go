// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func isCleanRailPool(ref cleanObjectRef) bool {
	return ref.GVK.Group == "spectrumx.nvidia.com" && ref.GVK.Kind == "SpectrumXRailPoolConfig"
}

// Spectrum-X creates SR-IOV policies and networks. Remove the producer first,
// while SR-IOV, NIC configuration and driver controllers still run. A fresh
// inventory at the gate catches policies created during final reconciliation.
func safeCleanResources(ctx context.Context, c client.Client, ns string, interval time.Duration, out ui.Output, readLogs sriovDaemonLogReader) (int, error) {
	if err := checkMOFEDCleanupTarget(ctx, c, ns); err != nil {
		return 0, err
	}
	refs, err := listCleanResources(ctx, c, ns)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for {
		reset, err := prepareSriovReset(ctx, c, refs, ns, readLogs)
		if err != nil {
			return deleted, err
		}
		var rails []cleanObjectRef
		for _, ref := range refs {
			if isCleanRailPool(ref) {
				rails = append(rails, ref)
			}
		}
		count, err := deleteAllAndWait(ctx, c, rails, interval, out, metav1.DeletePropagationForeground)
		deleted += count
		if err != nil {
			return deleted, err
		}
		if len(rails) > 0 {
			refs, err = listCleanResources(ctx, c, ns)
			if err != nil {
				return deleted, err
			}
			// A rail controller may create one last policy during deletion.
			// Capture its desired PFs without dropping the pre-delete baseline.
			if err := reset.refresh(ctx, c, refs); err != nil {
				return deleted, err
			}
		}
		out.Info("Waiting for network definitions and their generated attachments to be removed")
		count, err = cleanNetworkDefinitions(ctx, c, refs, ns, interval, out)
		deleted += count
		if err != nil {
			return deleted, err
		}
		out.Info("Waiting for NIC template intent to clear from NicDevices")
		count, err = cleanNICTemplates(ctx, c, refs, ns, interval, out)
		deleted += count
		if err != nil {
			return deleted, err
		}
		out.Info("Waiting for SR-IOV node states to reconcile the empty configuration")
		count, err = reset.deletePoliciesAndWait(ctx, c, interval, out)
		deleted += count
		if err != nil {
			return deleted, err
		}
		refs, err = listCleanResources(ctx, c, ns)
		if err != nil {
			return deleted, err
		}
		if slices.ContainsFunc(refs, func(ref cleanObjectRef) bool {
			return isCleanRailPool(ref) || isCleanNetworkDefinition(ref, ns) ||
				isCleanNICTemplate(ref, ns) || isCleanSriovIntent(ref, ns)
		}) {
			continue
		}
		// Other CRs can have interdependent finalizers. Signal all of them
		// before waiting, including the policies that own the driver DaemonSets.
		out.Info("Removing remaining custom resources")
		// A node can reboot during SR-IOV reset. Capture the current driver
		// instances only after that reset, immediately before deleting owners.
		count, err = withVerifiedMOFEDShutdown(ctx, c, ns, func(ctx context.Context) (int, error) {
			workloads, err := loadPolicyWorkloads(ctx, c, ns)
			if err != nil {
				return 0, err
			}
			if _, err := workloads.observeAndSave(ctx, c, ns); err != nil {
				return 0, err
			}
			count, err := deleteAllAndWait(ctx, c, refs, interval, out, metav1.DeletePropagationForeground)
			if err != nil {
				return count, err
			}
			return count, workloads.wait(ctx, c, ns, interval)
		})
		deleted += count
		if err != nil {
			return deleted, err
		}
		refs, err = listCleanResources(ctx, c, ns)
		if err != nil || len(refs) == 0 {
			return deleted, err
		}
	}
}
