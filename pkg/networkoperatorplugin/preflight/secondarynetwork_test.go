// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/secondarynetwork"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func removalPolicy(name string, managed bool) *unstructured.Unstructured {
	policy := newCR(secondarynetwork.PolicyGVK, "", name)
	policy.SetUID("policy-uid")
	if managed {
		policy.Object["spec"] = map[string]interface{}{"secondaryNetwork": map[string]interface{}{"multus": map[string]interface{}{"image": "multus-cni"}}}
	}
	return policy
}

func TestSecondaryNetworkRemovalPreflightUsesStrayScope(t *testing.T) {
	policy := removalPolicy("legacy", true)
	c := newFakeClientWith(t, policy).Build()
	in := Inputs{KubeClient: c, OperatorNamespace: "operator-system"}
	require.ErrorContains(t, CheckSecondaryNetworkRemovals(context.Background(), in), "refusing to remove")
	in.GeneratedManifests = []ObjectRef{{GVK: secondarynetwork.PolicyGVK, Name: policy.GetName()}}
	require.NoError(t, CheckSecondaryNetworkRemovals(context.Background(), in), "retained policy is not a deletion candidate")
	in.GeneratedManifests = nil
	in.OperatorNamespace = ""
	require.NoError(t, CheckSecondaryNetworkRemovals(context.Background(), in), "matches existing non-actionable stray check without namespace")
}

func TestSecondaryNetworkRemovalInspectionErrorsBlock(t *testing.T) {
	base := newFakeClientWith(t, removalPolicy("legacy", false)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("list denied")
	}})
	in := Inputs{KubeClient: c, OperatorNamespace: "operator-system"}
	require.ErrorContains(t, CheckSecondaryNetworkRemovals(context.Background(), in), "list denied")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, CheckSecondaryNetworkRemovals(ctx, in), context.Canceled)
	results := []Result{{Code: CodeStrayCRs, Mismatches: []Mismatch{{Path: "NicClusterPolicy/legacy", Detail: "mellanox.com/v1alpha1/NicClusterPolicy; cluster-scoped"}}}}
	deletes := 0
	c = interceptor.NewClient(base, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("get denied")
	}, Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		deletes++
		return nil
	}})
	require.ErrorContains(t, Remediate(context.Background(), Inputs{KubeClient: c}, results, RemediationOptions{}), "get denied")
	require.Zero(t, deletes)
	require.ErrorIs(t, Remediate(ctx, Inputs{KubeClient: c}, results, RemediationOptions{}), context.Canceled)
	require.Zero(t, deletes)
}

func TestSecondaryNetworkRemediationRefreshesEveryPolicyBeforeAnyDelete(t *testing.T) {
	policy := removalPolicy("legacy", false)
	network := newCR(schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}, "operator-system", "old-network")
	base := newFakeClientWith(t, policy, network).Build()
	in := Inputs{KubeClient: base, OperatorNamespace: "operator-system"}
	require.NoError(t, CheckSecondaryNetworkRemovals(context.Background(), in))
	checked := CheckStrayCRs(context.Background(), in)
	require.True(t, checked.Failed())
	live := removalPolicy("legacy", true)
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Name: "legacy"}, policy))
	live.SetResourceVersion(policy.GetResourceVersion())
	require.NoError(t, base.Update(context.Background(), live))
	// Put an ordinary network before the policy: no partial remediation may
	// occur before the late managed-policy conflict is discovered.
	checked.Mismatches = []Mismatch{{Path: "SriovNetwork/operator-system/old-network", Detail: "sriovnetwork.openshift.io/v1/SriovNetwork; namespaced"}, {Path: "NicClusterPolicy/legacy", Detail: "mellanox.com/v1alpha1/NicClusterPolicy; cluster-scoped"}}
	deletes := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		deletes++
		return nil
	}})
	err := Remediate(context.Background(), Inputs{KubeClient: c}, []Result{checked}, RemediationOptions{})
	require.ErrorContains(t, err, "refusing to remove")
	require.Zero(t, deletes)
}

func TestSecondaryNetworkRemediationPreservesOrdinaryStrayDeletion(t *testing.T) {
	policy := removalPolicy("legacy", false)
	base := newFakeClientWith(t, policy).Build()
	in := Inputs{KubeClient: base, OperatorNamespace: "operator-system"}
	require.NoError(t, CheckSecondaryNetworkRemovals(context.Background(), in))
	result := CheckStrayCRs(context.Background(), in)
	require.True(t, result.Failed())
	require.NoError(t, Remediate(context.Background(), in, []Result{result}, RemediationOptions{}))
	got, err := secondarynetwork.GetPolicy(context.Background(), base, "legacy")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSecondaryNetworkPolicyChangesAfterInspectionFailDeletePreconditions(t *testing.T) {
	base := newFakeClientWith(t, removalPolicy("legacy", false)).Build()
	in := Inputs{KubeClient: base, OperatorNamespace: "operator-system"}
	result := CheckStrayCRs(context.Background(), in)
	require.True(t, result.Failed())
	c := interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if obj.GetObjectKind().GroupVersionKind().GroupKind() == secondarynetwork.PolicyGVK.GroupKind() {
			changed := obj.(*unstructured.Unstructured).DeepCopy()
			changed.Object["spec"] = map[string]interface{}{"secondaryNetwork": map[string]interface{}{"multus": map[string]interface{}{"image": "multus-cni"}}}
			return c.Update(ctx, changed)
		}
		return nil
	}})
	err := Remediate(context.Background(), Inputs{KubeClient: c}, []Result{result}, RemediationOptions{})
	require.Error(t, err, "the checked resource version no longer authorizes deletion")
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(secondarynetwork.PolicyGVK)
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Name: "legacy"}, live))
	_, found, err := unstructured.NestedMap(live.Object, "spec", "secondaryNetwork", "multus")
	require.NoError(t, err)
	require.True(t, found)
}

func TestSecondaryNetworkRemovalTreatsEmptySectionAsManaged(t *testing.T) {
	policy := removalPolicy("legacy", false)
	policy.Object["spec"] = map[string]interface{}{"secondaryNetwork": map[string]interface{}{"multus": map[string]interface{}{}}}
	require.ErrorContains(t, secondarynetwork.CheckRemoval(policy), "managed multus")
	policy.Object["spec"] = map[string]interface{}{"secondaryNetwork": map[string]interface{}{"multus": nil}}
	require.NoError(t, secondarynetwork.CheckRemoval(policy))
}
