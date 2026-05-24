// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newScheme returns a runtime.Scheme with the kubernetes core types
// registered. Tests that need additional CRDs typically use unstructured
// + the fake client's automatic GVK handling; no extra registration is
// required for that case.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	return s
}

// newClient builds a fake client.Client seeded with the given objects.
// Pass unstructured objects directly — controller-runtime's fake client
// stores them by GVK without needing a typed scheme registration.
func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		Build()
}

// makeUnstructured constructs an *unstructured.Unstructured from the
// supplied apiVersion/kind + nested fields. setField accepts a slash-
// separated path and a leaf value: makeUnstructured(..., "status/state",
// "ready"). It's intentionally minimal — tests construct exactly the
// fields they need.
func makeUnstructured(apiVersion, kind, namespace, name string, fields map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	for path, val := range fields {
		_ = unstructured.SetNestedField(obj.Object, val, splitPath(path)...)
	}
	return obj
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, c := range p {
		if c == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestRegistry_FallbackExistence(t *testing.T) {
	manifest := makeUnstructured("v1", "ConfigMap", "ns", "cm", nil)
	live := manifest.DeepCopy()

	t.Run("found → success", func(t *testing.T) {
		c := newClient(t, live)
		r := NewRegistry()
		res, err := r.Validate(context.Background(), c, manifest)
		require.NoError(t, err)
		assert.Equal(t, StateSuccess, res.State)
	})

	t.Run("not-found → not-deployed", func(t *testing.T) {
		c := newClient(t)
		r := NewRegistry()
		res, err := r.Validate(context.Background(), c, manifest)
		require.NoError(t, err)
		assert.Equal(t, StateNotDeployed, res.State)
	})
}

func TestRegistry_RegisterOverrides(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "test.example.com", Version: "v1", Kind: "Widget"}
	r := NewRegistry()
	r.Register(gvk, func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
		return Result{State: StateInProgress, Reason: "stub"}, nil
	})

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("w")
	res, err := r.Validate(context.Background(), newClient(t), obj)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Equal(t, "stub", res.Reason)
}

func TestRegistry_NilObjectErrors(t *testing.T) {
	r := NewDefault()
	_, err := r.Validate(context.Background(), newClient(t), nil)
	assert.Error(t, err)
}

func TestStatusStringValidator_AllStates(t *testing.T) {
	manifest := makeUnstructured("mellanox.com/v1alpha1", "NicClusterPolicy", "", "nic-cluster-policy", nil)

	cases := []struct {
		name      string
		stateStr  string
		reasonStr string
		want      CRState
	}{
		{"ready→success", netopStateReady, "", StateSuccess},
		{"ignore→success", netopStateIgnore, "", StateSuccess},
		{"notReady→in-progress", netopStateNotReady, "still reconciling", StateInProgress},
		{"error→error", netopStateError, "boom", StateError},
		{"empty→in-progress", "", "", StateInProgress},
		{"unknown→in-progress", "weird", "", StateInProgress},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := manifest.DeepCopy()
			if tc.stateStr != "" {
				_ = unstructured.SetNestedField(live.Object, tc.stateStr, "status", "state")
			}
			if tc.reasonStr != "" {
				_ = unstructured.SetNestedField(live.Object, tc.reasonStr, "status", "reason")
			}
			r := NewDefault()
			c := newClient(t, live)
			res, err := r.Validate(context.Background(), c, manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.State)
			if tc.want == StateError {
				assert.Equal(t, tc.reasonStr, res.Reason)
			}
		})
	}

	t.Run("missing→not-deployed", func(t *testing.T) {
		r := NewDefault()
		c := newClient(t)
		res, err := r.Validate(context.Background(), c, manifest)
		require.NoError(t, err)
		assert.Equal(t, StateNotDeployed, res.State)
	})
}

func TestStatusStringValidator_RegisteredKinds(t *testing.T) {
	// Make sure every Kind listed in the plan is wired up — a regression
	// in registerStatusStringValidators would silently fall through to
	// the existence-only fallback and miss controller-reported errors.
	r := NewDefault()
	for _, kind := range []string{"NicClusterPolicy", "NicNodePolicy", "HostDeviceNetwork", "IPoIBNetwork", "MacvlanNetwork"} {
		gvk := schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind}
		_, ok := r.validators[gvk]
		assert.Truef(t, ok, "%s not registered", kind)
	}
}

func TestNeedsObservationGate(t *testing.T) {
	cases := []struct {
		gvk  schema.GroupVersionKind
		want bool
		name string
	}{
		// Kinds whose validators read .status on the CR itself —
		// gate is meaningful, controller's RV bump is the signal.
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"}, true, "NicClusterPolicy"},
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicNodePolicy"}, true, "NicNodePolicy"},
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "HostDeviceNetwork"}, true, "HostDeviceNetwork"},
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "IPoIBNetwork"}, true, "IPoIBNetwork"},
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "MacvlanNetwork"}, true, "MacvlanNetwork"},
		{schema.GroupVersionKind{Group: "spectrumx.nvidia.com", Version: "v1alpha2", Kind: "SpectrumXRailPoolConfig"}, true, "SpectrumXRailPoolConfig"},

		// Kinds whose validators read companion CRs — gate would
		// block forever waiting for an RV bump that never lands.
		{schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetworkNodePolicy"}, false, "SriovNetworkNodePolicy (companion = SriovNetworkNodeState)"},
		{schema.GroupVersionKind{Group: "configuration.net.nvidia.com", Version: "v1alpha1", Kind: "NicInterfaceNameTemplate"}, false, "NicInterfaceNameTemplate (companion = NicDevice)"},
		{schema.GroupVersionKind{Group: "configuration.net.nvidia.com", Version: "v1alpha1", Kind: "NicConfigurationTemplate"}, false, "NicConfigurationTemplate (companion = NicDevice)"},

		// Existence-only Kinds — no status at all, gate irrelevant.
		{schema.GroupVersionKind{Group: "nv-ipam.nvidia.com", Version: "v1alpha1", Kind: "IPPool"}, false, "IPPool"},
		{schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}, false, "SriovNetwork"},

		// Wrong group / version — never gate.
		{schema.GroupVersionKind{Group: "mellanox.com", Version: "v1beta1", Kind: "NicClusterPolicy"}, false, "wrong version"},
		{schema.GroupVersionKind{Group: "other.example.com", Version: "v1", Kind: "NicClusterPolicy"}, false, "wrong group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NeedsObservationGate(tc.gvk))
		})
	}
}

// node returns a labelled *corev1.Node for fake-client seeding.
func node(name string, labels map[string]string) *corev1.Node {
	n := &corev1.Node{}
	n.Name = name
	n.Labels = labels
	return n
}

// appliedState builds one entry for .status.appliedStates[] in the
// shape the Network Operator writes (name+state+message).
func appliedState(name, state, message string) map[string]interface{} {
	m := map[string]interface{}{
		"name":  name,
		"state": state,
	}
	if message != "" {
		m["message"] = message
	}
	return m
}

func TestStatusStringValidator_NCPAppliedStates(t *testing.T) {
	manifest := makeUnstructured("mellanox.com/v1alpha1", "NicClusterPolicy", "", "nic-cluster-policy", nil)

	t.Run("notReady surfaces pending component names", func(t *testing.T) {
		live := manifest.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, netopStateNotReady, "status", "state")
		states := []interface{}{
			appliedState("multus", netopStateReady, ""),
			appliedState("cni-plugins", netopStateReady, ""),
			appliedState("ofed-driver", netopStateNotReady, "waiting for daemonset rollout"),
			appliedState("sriov-device-plugin", netopStateNotReady, ""),
		}
		_ = unstructured.SetNestedSlice(live.Object, states, "status", "appliedStates")

		r := NewDefault()
		res, err := r.Validate(context.Background(), newClient(t, live), manifest)
		require.NoError(t, err)
		assert.Equal(t, StateInProgress, res.State)
		assert.Contains(t, res.Reason, "ready: 2/4")
		assert.Contains(t, res.Reason, "pending: ofed-driver, sriov-device-plugin")
		// Details should carry the per-component breakdown so JSON
		// consumers don't have to re-parse the Reason string.
		assert.Equal(t, "ready", res.Details["multus"])
		assert.Equal(t, "notReady: waiting for daemonset rollout", res.Details["ofed-driver"])
	})

	t.Run("ready summary lists healthy components", func(t *testing.T) {
		live := manifest.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, netopStateReady, "status", "state")
		states := []interface{}{
			appliedState("multus", netopStateReady, ""),
			appliedState("cni-plugins", netopStateReady, ""),
		}
		_ = unstructured.SetNestedSlice(live.Object, states, "status", "appliedStates")

		r := NewDefault()
		res, err := r.Validate(context.Background(), newClient(t, live), manifest)
		require.NoError(t, err)
		assert.Equal(t, StateSuccess, res.State)
		assert.Contains(t, res.Reason, "ready: 2/2")
		assert.Contains(t, res.Reason, "components: cni-plugins, multus")
	})

	t.Run("error bubbles up errored component message", func(t *testing.T) {
		live := manifest.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, netopStateError, "status", "state")
		_ = unstructured.SetNestedField(live.Object, "see component", "status", "reason")
		states := []interface{}{
			appliedState("ofed-driver", netopStateError, "image pull failed"),
			appliedState("multus", netopStateReady, ""),
		}
		_ = unstructured.SetNestedSlice(live.Object, states, "status", "appliedStates")

		r := NewDefault()
		res, err := r.Validate(context.Background(), newClient(t, live), manifest)
		require.NoError(t, err)
		assert.Equal(t, StateError, res.State)
		assert.Contains(t, res.Reason, "ready: 1/2")
		assert.Contains(t, res.Reason, "error: ofed-driver (image pull failed)")
	})

	t.Run("no appliedStates falls back to plain state", func(t *testing.T) {
		// Network kinds (HostDeviceNetwork, IPoIBNetwork, MacvlanNetwork)
		// don't carry appliedStates — the validator must still classify
		// the bare .status.state.
		net := makeUnstructured("mellanox.com/v1alpha1", "HostDeviceNetwork", "", "net", nil)
		live := net.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, netopStateReady, "status", "state")

		r := NewDefault()
		res, err := r.Validate(context.Background(), newClient(t, live), net)
		require.NoError(t, err)
		assert.Equal(t, StateSuccess, res.State)
		assert.Equal(t, "ready", res.Reason)
		assert.Empty(t, res.Details)
	})
}
