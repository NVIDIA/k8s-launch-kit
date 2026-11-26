// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"slices"
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
	var sriov *ocpSriovClean
	var nics *ocpNICClean
	var workloads *ocpPolicyWorkloadClean
	var err error
	if !opts.SkipWait {
		// Capture live worker/controller scope before removing its parent intent.
		sriov, err = prepareOCPSriovClean(ctx, c, refs)
		if err != nil {
			return 0, err
		}
		nics, err = prepareOCPNICClean(ctx, c, refs)
		if err != nil {
			return 0, err
		}
		workloads, err = prepareOCPWorkloadClean(ctx, c, opts.Namespace, refs)
		if err != nil {
			return 0, err
		}
		if err := workloads.checkDriverNamespaces(ctx, c); err != nil {
			return 0, err
		}
	}

	// Deployment applies the cluster root, node roots, then the remaining
	// manifests in bundle order. Reverse that sequence without mutating the plan.
	ordered := slices.Clone(refs)
	slices.Reverse(ordered)
	others, roots := partitionOCPRootPolicies(ordered)
	var networks []cleanObjectRef
	for _, ref := range others {
		if isCleanNetworkDefinition(ref, ref.Namespace) {
			networks = append(networks, ref)
		}
	}
	// Missing parents are still exact generated-NAD observation targets on retry.
	if _, err := ocpNetworkAttachments(ctx, c, networks); err != nil {
		return 0, err
	}
	deleted := 0
	for _, ref := range others {
		if ref.GVK.Group == "nv-ipam.nvidia.com" && ref.GVK.Kind == "IPPool" {
			live, err := inspectOCPDeletionTarget(ctx, c, ref)
			if err != nil {
				return deleted, err
			}
			if live == nil {
				continue
			}
			if err := checkOCPPoolConsumers(ctx, c, cfg, []cleanObjectRef{ref}); err != nil {
				return deleted, err
			}
		}
		n, err := deleteOCPRefs(ctx, c, []cleanObjectRef{ref}, opts.PollInterval)
		deleted += n
		if err != nil {
			return deleted, err
		}
		if isCleanNetworkDefinition(ref, ref.Namespace) {
			// This is a dependency/finalizer barrier even with --skip-wait: a
			// Delete response does not mean the generated NAD has disappeared.
			if err := pollOCPClean(ctx, opts.PollInterval, "selected generated network attachment", func(ctx context.Context) (string, error) {
				pending, err := ocpNetworkAttachments(ctx, c, []cleanObjectRef{ref})
				return joinOCPCleanReasons(pending), err
			}); err != nil {
				return deleted, err
			}
		}
	}
	if !opts.SkipWait {
		// Several templates/policies may share a device or PF. Remove every
		// selected intent before waiting for their combined reset.
		if err := nics.wait(ctx, c, opts.PollInterval); err != nil {
			return deleted, err
		}
		if err := sriov.wait(ctx, c, opts.PollInterval); err != nil {
			return deleted, err
		}
	}
	removePolicies := func(ctx context.Context) (int, error) {
		if workloads != nil {
			if _, err := workloads.observe(ctx, c); err != nil {
				return 0, err
			}
		}
		// Worker reconciliation can take minutes; refresh root dependencies at
		// the boundary where their shared controllers would actually be removed.
		if err := checkOCPCleanDependencies(ctx, c, cfg, refs); err != nil {
			return 0, err
		}
		// Signal every node/cluster root before waiting on cross-root finalizers.
		n, err := deleteOCPRefs(ctx, c, roots, opts.PollInterval)
		if err != nil || workloads == nil {
			return n, err
		}
		err = pollOCPClean(ctx, opts.PollInterval, "selected policy workloads", func(ctx context.Context) (string, error) {
			pending, err := workloads.observe(ctx, c)
			return joinOCPCleanReasons(pending), err
		})
		return n, err
	}
	var n int
	if workloads != nil && len(workloads.roots) > 0 {
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
	if nics != nil && len(nics.devices) > 0 {
		out.Info("Selected NIC template intent cleared; template deletion does not restore firmware/NV configuration and the API does not acknowledge host udev-rule removal")
	}
	if sriov != nil && len(sriov.nodes) > 0 {
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

// Preserve reverse deployment order while separating the roots that must all
// receive deletion before waiting on cross-policy finalizers.
func partitionOCPRootPolicies(refs []cleanObjectRef) (others, roots []cleanObjectRef) {
	for _, ref := range refs {
		if isCleanRootPolicy(ref.GVK) {
			roots = append(roots, ref)
		} else {
			others = append(others, ref)
		}
	}
	return others, roots
}
