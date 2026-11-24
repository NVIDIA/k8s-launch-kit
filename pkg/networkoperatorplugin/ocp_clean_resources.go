// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cleanOCPResources consumes the preflighted desired identities. Missing roots
// remain observation targets on retries; they never authorize a name-only delete.
// External platform operators and controller-generated states are never deleted.
func cleanOCPResources(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, refs []cleanObjectRef, opts CleanOptions) (int, error) {
	return cleanOCPResourcesWithLogs(ctx, c, cfg, refs, opts, newSriovDaemonLogReader(opts.RestConfig))
}

func cleanOCPResourcesWithLogs(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, refs []cleanObjectRef, opts CleanOptions, readLogs sriovDaemonLogReader) (int, error) {
	if ctx == nil || c == nil || cfg == nil {
		return 0, fmt.Errorf("clean OpenShift resources: context, client and config are required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultCleanPollInterval
	}
	if len(refs) == 0 {
		return 0, nil
	}
	if opts.Namespace == "" {
		return 0, fmt.Errorf("clean OpenShift resources: operator namespace is required")
	}
	if err := checkOCPCleanDependencies(ctx, c, cfg, refs); err != nil {
		return 0, err
	}
	out := ui.FromContext(ctx)
	receipt, verified, err := loadOCPCleanReceipt(ctx, c, opts.Namespace, refs)
	if err != nil {
		return 0, err
	}
	completedReceipt := verified && (receipt.object.Data["state"] == "complete" || receipt.object.Data["state"] == "resources-complete")
	if opts.SkipWait {
		// An explicit evidence waiver cannot create reusable hardware proof.
		if !completedReceipt {
			if err := receipt.save(ctx, c, "cleanup-active"); err != nil {
				return 0, err
			}
		}
		others, roots := partitionOCPRootPolicies(refs)
		var nonPools, pools []cleanObjectRef
		for _, ref := range others {
			if ref.GVK.Group == "nv-ipam.nvidia.com" && ref.GVK.Kind == "IPPool" {
				pools = append(pools, ref)
			} else {
				nonPools = append(nonPools, ref)
			}
		}
		n, err := deleteOCPRefs(ctx, c, nonPools, opts.PollInterval)
		if err != nil {
			return n, err
		}
		if err := checkOCPPoolConsumers(ctx, c, cfg, pools); err != nil {
			return n, err
		}
		count, err := deleteOCPRefs(ctx, c, pools, opts.PollInterval)
		n += count
		if err != nil {
			return n, err
		}
		count, err = deleteOCPRefs(ctx, c, roots, opts.PollInterval)
		n += count
		if err != nil {
			return n, err
		}
		state := "resources-unverified"
		if completedReceipt {
			state = "resources-complete"
		}
		return n, receipt.save(ctx, c, state)
	}
	if err := requireOCPMissingRootProof(ctx, c, refs, receipt, verified); err != nil {
		return 0, err
	}
	// With no surviving target and no earlier receipt, parent disappearance
	// has also erased the ancestry needed to qualify orphaned workload teardown.
	// An absence inventory cannot manufacture historical reconciliation evidence.
	if receipt.object.ResourceVersion == "" {
		allAbsent := true
		for _, ref := range refs {
			if ref.UID != "" {
				allAbsent = false
				break
			}
		}
		if allAbsent {
			return 0, fmt.Errorf("all selected OpenShift resources are absent but no cleanup verification receipt exists; historical worker and workload teardown cannot be verified; inspect affected hosts and use --skip-wait only to explicitly waive that evidence")
		}
	}
	// Claim the shared-restoration stage on an already completed retry. The
	// resource proof remains complete; a concurrent deployment's CAS invalidation
	// will conflict with this update before either side performs shared writes.
	if completedReceipt {
		if err := receipt.save(ctx, c, "resources-complete"); err != nil {
			return 0, err
		}
	}
	var sriov *ocpSriovClean
	var nics *ocpNICClean
	if !verified {
		// Acquire every hardware baseline before the first deletion. A durable
		// receipt preserves these checks through later policy/controller teardown.
		sriov, err = prepareOCPSriovClean(ctx, c, refs, readLogs)
		if err != nil {
			return 0, err
		}
		nics, err = prepareOCPNICClean(ctx, c, refs)
		if err != nil {
			return 0, err
		}
	}
	workloads, err := prepareOCPWorkloadClean(ctx, c, opts.Namespace, refs, !completedReceipt)
	if err != nil {
		return 0, err
	}
	if len(workloads.roots) > 0 {
		if !completedReceipt {
			receipt.ancestryCheckpoint = workloads.checkpoint.Name
			receipt.ancestryUID = workloads.checkpoint.UID
		}
		if err := workloads.checkDriverNamespaces(ctx, c); err != nil {
			return 0, err
		}
		if err := checkMOFEDCleanupCheckpoint(ctx, c, opts.Namespace, workloads.mofedCheckpoint()); err != nil {
			return 0, err
		}
	}
	var networks, templates, policies, remaining, roots []cleanObjectRef
	for _, ref := range refs {
		switch {
		case isCleanNetworkDefinition(ref, ref.Namespace):
			networks = append(networks, ref)
		case isCleanNICTemplate(ref, ref.Namespace):
			templates = append(templates, ref)
		case isCleanSriovIntent(ref, ref.Namespace):
			policies = append(policies, ref)
		case isCleanRootPolicy(ref.GVK):
			roots = append(roots, ref)
		default:
			remaining = append(remaining, ref)
		}
	}
	// Preflight NAD visibility as well: permission failure must precede deletion.
	if _, err := ocpNetworkAttachments(ctx, c, networks); err != nil {
		return 0, err
	}
	if !verified {
		if err := receipt.save(ctx, c, "cleanup-active"); err != nil {
			return 0, err
		}
	}
	deleted := 0
	run := func(selected []cleanObjectRef) error {
		n, err := deleteOCPRefs(ctx, c, selected, opts.PollInterval)
		deleted += n
		return err
	}
	if err := run(networks); err != nil {
		return deleted, err
	}
	if err := pollOCPClean(ctx, opts.PollInterval, "selected generated network attachments", func(ctx context.Context) (string, error) {
		pending, err := ocpNetworkAttachments(ctx, c, networks)
		return joinOCPCleanReasons(pending), err
	}); err != nil {
		return deleted, err
	}
	if err := run(templates); err != nil {
		return deleted, err
	}
	if !verified {
		if err := nics.wait(ctx, c, opts.PollInterval); err != nil {
			return deleted, err
		}
	}
	if err := run(policies); err != nil {
		return deleted, err
	}
	if !verified {
		if err := sriov.wait(ctx, c, opts.PollInterval); err != nil {
			return deleted, err
		}
	}
	if !verified {
		if err := receipt.save(ctx, c, "hardware-verified"); err != nil {
			return deleted, err
		}
	}
	// Clear IP pools and other remaining CR finalizers while their policy-owned
	// controllers are alive. Only the root-policy stage can remove those workloads.
	if err := checkOCPPoolConsumers(ctx, c, cfg, ocpCleanPoolRefs(remaining)); err != nil {
		return deleted, err
	}
	if err := run(remaining); err != nil {
		return deleted, err
	}
	_, roots = partitionOCPRootPolicies(roots)
	// Driver shutdown is observed continuously, starting before the selected root
	// policies disappear. Unrelated policies' controllers and drivers may remain.
	removePolicies := func(ctx context.Context) (int, error) {
		if _, err := workloads.observe(ctx, c); err != nil {
			return 0, err
		}
		n, err := deleteOCPRefs(ctx, c, roots, opts.PollInterval)
		if err != nil {
			return n, err
		}
		err = pollOCPClean(ctx, opts.PollInterval, "selected policy workloads", func(ctx context.Context) (string, error) {
			pending, err := workloads.observe(ctx, c)
			return joinOCPCleanReasons(pending), err
		})
		return n, err
	}
	var n int
	if len(workloads.roots) > 0 && !completedReceipt {
		n, err = withScopedMOFEDShutdown(ctx, c, opts.Namespace, workloads.mofedScope(ctx, c, refs), removePolicies)
	} else {
		n, err = removePolicies(ctx)
	}
	deleted += n
	if err != nil {
		return deleted, err
	}
	if err := ocpRefsAbsent(ctx, c, refs); err != nil {
		return deleted, err
	}
	// Persist terminal resource proof before discarding its ancestry journal.
	// A failed checkpoint removal can then be retried without withdrawn APIs.
	if err := receipt.save(ctx, c, "resources-complete"); err != nil {
		return deleted, err
	}
	if err := workloads.removeCheckpoint(ctx, c); err != nil {
		return deleted, err
	}
	if len(templates) > 0 {
		out.Info("Selected NIC template intent cleared; template deletion does not restore firmware/NV configuration and the API does not acknowledge host udev-rule removal")
	}
	if len(policies) > 0 {
		out.Info("Selected SR-IOV PFs report zero VFs and cleared policy intent; firmware defaults and unrelated host configuration are outside this API verification")
	}
	return deleted, nil
}

// Recheck the whole approved inventory before teardown and each individual
// identity again just before issuing its conditional deletion.
func inspectOCPDeletionTarget(ctx context.Context, c client.Client, ref cleanObjectRef) (*unstructured.Unstructured, error) {
	live := objectForCleanRef(ref)
	if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("recheck %s before deletion: %w", cleanRefString(ref), err)
	}
	if ref.UID == "" || live.GetUID() != ref.UID {
		return nil, fmt.Errorf("refuse deletion of %s: resource appeared or was replaced after ownership preflight", cleanRefString(ref))
	}
	if err := validateOCPCleanLiveOwnership(ctx, c, ref.Desired, live); err != nil {
		return nil, fmt.Errorf("refuse deletion of %s: %w", cleanRefString(ref), err)
	}
	if live.GetResourceVersion() == "" {
		return nil, fmt.Errorf("refuse deletion of %s: resource version is missing", cleanRefString(ref))
	}

	return live, nil
}

func deleteOCPRefs(ctx context.Context, c client.Client, refs []cleanObjectRef, interval time.Duration) (int, error) {
	deleted := 0
	for _, ref := range refs {
		live, err := inspectOCPDeletionTarget(ctx, c, ref)
		if err != nil {
			return deleted, err
		}
		if live == nil {
			continue
		}
		propagation := metav1.DeletePropagationForeground
		if isCleanRootPolicy(ref.GVK) {
			propagation = metav1.DeletePropagationBackground
		}
		uid, version := live.GetUID(), live.GetResourceVersion()
		if err := c.Delete(ctx, live, client.PropagationPolicy(propagation), client.Preconditions{UID: &uid, ResourceVersion: &version}); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return deleted, fmt.Errorf("delete %s: %w", cleanRefString(ref), err)
		}
		deleted++
	}
	err := pollOCPClean(ctx, interval, "selected custom resource deletion", func(ctx context.Context) (string, error) {
		var pending []string
		for _, ref := range refs {
			live := objectForCleanRef(ref)
			if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			} else if err != nil {
				return "", fmt.Errorf("observe %s deletion: %w", cleanRefString(ref), err)
			}
			if ref.UID == "" || live.GetUID() != ref.UID {
				return "", fmt.Errorf("%s appeared or was replaced during cleanup; refusing to follow its new identity", cleanRefString(ref))
			}
			pending = append(pending, cleanRefString(ref))
		}
		return joinOCPCleanReasons(pending), nil
	})
	return deleted, err
}

func ocpRefsAbsent(ctx context.Context, c client.Client, refs []cleanObjectRef) error {
	for _, ref := range refs {
		live := objectForCleanRef(ref)
		if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("final cleanup observation of %s: %w", cleanRefString(ref), err)
		}
		return fmt.Errorf("%s reappeared during cleanup; selected teardown remains incomplete", cleanRefString(ref))
	}
	return nil
}

func pollOCPClean(ctx context.Context, interval time.Duration, what string, observe func(context.Context) (string, error)) error {
	pending := "observation incomplete"
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		var err error
		pending, err = observe(ctx)
		return pending == "", err
	})
	if err != nil {
		return fmt.Errorf("wait for %s (%s): %w", what, pending, err)
	}
	return nil
}

func ocpDesired(ref cleanObjectRef) (*unstructured.Unstructured, error) {
	if ref.Desired == nil {
		return nil, fmt.Errorf("clean %s: retained desired object is required for scoped teardown", cleanRefString(ref))
	}
	return ref.Desired, nil
}

// Signal node policies before their shared NCP, but wait only after every root
// has received deletion so cross-policy finalizers cannot deadlock the teardown.
func partitionOCPRootPolicies(refs []cleanObjectRef) (others, roots []cleanObjectRef) {
	for _, ref := range refs {
		if isCleanRootPolicy(ref.GVK) {
			roots = append(roots, ref)
		} else {
			others = append(others, ref)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool {
		return roots[i].GVK.Kind == "NicNodePolicy" && roots[j].GVK.Kind == "NicClusterPolicy"
	})
	return others, roots
}

func ocpCleanPoolRefs(refs []cleanObjectRef) []cleanObjectRef {
	var pools []cleanObjectRef
	for _, ref := range refs {
		if ref.GVK.Group == "nv-ipam.nvidia.com" && ref.GVK.Kind == "IPPool" {
			pools = append(pools, ref)
		}
	}
	return pools
}
