// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestOpenShiftPolicyReadiness(t *testing.T) {
	states := func(remaining ...interface{}) []interface{} {
		return append([]interface{}{
			appliedState("state-multus-cni", netopStateError, "platform managed"),
			appliedState("state-container-networking-plugins", netopStateNotReady, "platform managed"),
		}, remaining...)
	}
	ready := appliedState("state-nv-ipam-cni", netopStateReady, "")
	for _, tc := range []struct {
		name    string
		states  []interface{}
		live    map[string]interface{}
		desired map[string]interface{}
		want    CRState
		adapted bool
	}{
		{name: "external states alone block aggregate", states: states(ready), want: StateSuccess, adapted: true},
		{name: "external states cause aggregate error", states: states(ready), live: map[string]interface{}{"status/state": netopStateError}, want: StateSuccess, adapted: true},
		{name: "managed error keeps aggregate progress", states: states(appliedState("state-nv-ipam-cni", netopStateError, "image pull failed")), want: StateInProgress, adapted: true},
		{name: "managed error keeps aggregate error", states: states(appliedState("state-nv-ipam-cni", netopStateError, "image pull failed")), live: map[string]interface{}{"status/state": netopStateError}, want: StateError, adapted: true},
		{name: "managed rollout remains pending", states: states(appliedState("state-nv-ipam-cni", netopStateNotReady, "rolling out")), want: StateInProgress, adapted: true},
		{name: "configured component is absent from status", states: states(), want: StateInProgress, adapted: true},
		{name: "configured component reports ignore", states: states(appliedState("state-nv-ipam-cni", netopStateIgnore, "")), want: StateInProgress, adapted: true},
		{name: "desired component not yet live", states: states(ready), desired: map[string]interface{}{"spec/nicConfigurationOperator": map[string]interface{}{}}, want: StateInProgress, adapted: true},
		{name: "live component coverage also required", states: states(ready), live: map[string]interface{}{"spec/ofedDriver": map[string]interface{}{}}, want: StateInProgress, adapted: true},
		{name: "IPoIB remains managed", states: states(ready), live: map[string]interface{}{"spec/secondaryNetwork/ipoib": map[string]interface{}{}}, want: StateInProgress, adapted: true},
		{name: "IPoIB error still blocks", states: states(ready, appliedState("state-ipoib-cni", netopStateError, "CNI installation failed")), live: map[string]interface{}{"spec/secondaryNetwork/ipoib": map[string]interface{}{}}, want: StateInProgress, adapted: true},
		{name: "unknown nonexternal failure still blocks", states: states(ready, appliedState("future-state", netopStateError, "failed")), want: StateInProgress, adapted: true},
		{name: "unknown nonexternal status still waits", states: states(ready, appliedState("future-state", "unknown", "")), want: StateInProgress, adapted: true},
		{name: "unknown external Multus state", states: []interface{}{appliedState("state-multus-cni", "unknown", ""), ready}, want: StateInProgress},
		{name: "unknown external plugin state", states: []interface{}{appliedState("state-container-networking-plugins", "unknown", ""), ready}, live: map[string]interface{}{"status/state": netopStateError}, want: StateError},
		{name: "missing external state", states: []interface{}{map[string]interface{}{"name": "state-multus-cni"}, ready}, want: StateInProgress},
		{name: "null external sections remain external", states: states(ready), live: map[string]interface{}{"spec/secondaryNetwork": map[string]interface{}{"multus": nil, "cniPlugins": nil}}, want: StateSuccess, adapted: true},
		{name: "unrelated policy reason preserved", states: states(ready), live: map[string]interface{}{"status/state": netopStateError, "status/reason": "policy reconciliation failed"}, want: StateError},
		{name: "unknown spec coverage cannot establish readiness", states: states(ready), live: map[string]interface{}{"spec/futureComponent": map[string]interface{}{}}, want: StateInProgress},
		{name: "malformed component spec", states: states(ready), live: map[string]interface{}{"spec/nvIpam": "invalid"}, want: StateInProgress},
		{name: "malformed secondary spec", states: states(ready), live: map[string]interface{}{"spec/secondaryNetwork": "invalid"}, want: StateInProgress},
		{name: "malformed IPoIB spec", states: states(ready), live: map[string]interface{}{"spec/secondaryNetwork/ipoib": "invalid"}, want: StateInProgress},
		{name: "empty Multus mapping expresses ownership", states: states(ready), live: map[string]interface{}{"spec/secondaryNetwork/multus": map[string]interface{}{}}, want: StateInProgress},
		{name: "empty plugins mapping expresses ownership", states: states(ready), desired: map[string]interface{}{"spec/secondaryNetwork/cniPlugins": map[string]interface{}{}}, want: StateInProgress},
		{name: "malformed status entry", states: states(ready, "invalid"), want: StateInProgress},
		{name: "duplicate status entry", states: states(ready, ready), want: StateInProgress},
		{name: "no external failing state to explain aggregate", states: []interface{}{ready, appliedState("state-multus-cni", netopStateIgnore, "")}, want: StateInProgress},
		{name: "no component status evidence", live: map[string]interface{}{"status/state": netopStateError}, want: StateError},
		{name: "unknown aggregate remains unverified", states: states(ready), live: map[string]interface{}{"status/state": "unknown"}, want: StateInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := makeUnstructured("mellanox.com/v1alpha1", "NicClusterPolicy", "", "nic-cluster-policy", map[string]interface{}{
				"spec/nvIpam": map[string]interface{}{},
			})
			live := desired.DeepCopy()
			live.SetResourceVersion("1")
			require.NoError(t, unstructured.SetNestedField(live.Object, netopStateNotReady, "status", "state"))
			if tc.states != nil {
				require.NoError(t, unstructured.SetNestedSlice(live.Object, tc.states, "status", "appliedStates"))
			}
			for path, value := range tc.live {
				require.NoError(t, unstructured.SetNestedField(live.Object, value, splitPath(path)...))
			}
			for path, value := range tc.desired {
				require.NoError(t, unstructured.SetNestedField(desired.Object, value, splitPath(path)...))
			}
			originalLive, originalDesired := live.DeepCopy(), desired.DeepCopy()
			c := newClient(t, live)
			result, err := NewOpenShift().Validate(context.Background(), c, desired)
			require.NoError(t, err)
			require.Equal(t, tc.want, result.State)
			if tc.adapted {
				require.Contains(t, result.Reason, "excludes OpenShift-managed Multus/CNI states")
				require.Contains(t, result.Details["state-multus-cni"], "UNVERIFIED")
			} else {
				require.NotContains(t, result.Reason, "excludes OpenShift-managed Multus/CNI states")
			}
			plain, err := NewDefault().Validate(context.Background(), c, desired)
			require.NoError(t, err)
			wantPlain := StateInProgress
			if tc.live["status/state"] == netopStateError {
				wantPlain = StateError
			}
			require.Equal(t, wantPlain, plain.State, "Kubernetes classification must remain unchanged")
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(live.GroupVersionKind())
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(live), got))
			require.Equal(t, originalLive.Object, got.Object, "classification must not rewrite live status")
			require.Equal(t, originalDesired.Object, desired.Object, "classification must not mutate desired artifacts")
		})
	}
}

func TestOpenShiftPolicyReadErrorsAndOtherKinds(t *testing.T) {
	desired := makeUnstructured("mellanox.com/v1alpha1", "NicClusterPolicy", "", "nic-cluster-policy", nil)
	r := NewOpenShift()
	result, err := r.Validate(context.Background(), newClient(t), desired)
	require.NoError(t, err)
	require.Equal(t, StateNotDeployed, result.State)
	denied := errors.New("get denied")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return denied
		},
	}).Build()
	result, err = r.Validate(context.Background(), c, desired)
	require.ErrorIs(t, err, denied)
	require.Equal(t, StateError, result.State)

	nnp := makeUnstructured("mellanox.com/v1alpha1", "NicNodePolicy", "", "worker-policy", map[string]interface{}{
		"spec/nvIpam": map[string]interface{}{}, "status/state": netopStateNotReady,
		"status/appliedStates": []interface{}{appliedState("state-multus-cni", netopStateError, ""), appliedState("state-nv-ipam-cni", netopStateReady, "")},
	})
	result, err = r.Validate(context.Background(), newClient(t, nnp), nnp)
	require.NoError(t, err)
	require.Equal(t, StateInProgress, result.State, "OpenShift overrides only the NCP validator")
}
