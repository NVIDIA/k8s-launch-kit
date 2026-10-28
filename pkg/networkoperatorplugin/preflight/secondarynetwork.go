// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/secondarynetwork"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CheckSecondaryNetworkRemovals inspects prospective NCP stray deletions before
// Helm can mutate the cluster. Only overwrite deployment calls it. Its scope is
// identical to CheckStrayCRs: no namespace means that check cannot remediate,
// and rendered identities are retained. No installer DaemonSet is required to
// establish a policy's managed ownership intent.
func CheckSecondaryNetworkRemovals(ctx context.Context, in Inputs) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.OperatorNamespace == "" {
		return nil
	}
	if in.KubeClient == nil {
		return fmt.Errorf("secondary-network removal inspection requires a Kubernetes client")
	}
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(secondarynetwork.PolicyGVK.GroupVersion().WithKind("NicClusterPolicyList"))
	if err := in.KubeClient.List(ctx, policies); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("inspect NicClusterPolicy deletion candidates: %w", err)
	}
	expected := expectedRefSet(in.GeneratedManifests)
	for i := range policies.Items {
		policy := &policies.Items[i]
		ref := ObjectRef{GVK: secondarynetwork.PolicyGVK, Namespace: policy.GetNamespace(), Name: policy.GetName()}
		if _, retained := expected[refKey(ref)]; retained {
			continue
		}
		if err := secondarynetwork.CheckRemoval(policy); err != nil {
			return err
		}
	}
	return ctx.Err()
}
