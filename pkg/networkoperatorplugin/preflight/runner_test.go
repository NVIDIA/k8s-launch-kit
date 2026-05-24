// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResult_PassedFailed(t *testing.T) {
	r := Result{Code: "X"}
	assert.True(t, r.Passed())
	assert.False(t, r.Failed())

	r.Skipped = true
	assert.False(t, r.Passed())
	assert.False(t, r.Failed())

	r = Result{Code: "X", Mismatches: []Mismatch{{Path: "p"}}}
	assert.False(t, r.Passed())
	assert.True(t, r.Failed())
}

func TestAnyFailed(t *testing.T) {
	assert.False(t, AnyFailed(nil))
	assert.False(t, AnyFailed([]Result{{Code: "A", Skipped: true}}))
	assert.False(t, AnyFailed([]Result{{Code: "A"}}))
	assert.True(t, AnyFailed([]Result{
		{Code: "A"},
		{Code: "B", Mismatches: []Mismatch{{Path: "p"}}},
	}))
}

func TestFailedCodes(t *testing.T) {
	out := FailedCodes([]Result{
		{Code: "A"},
		{Code: "B", Mismatches: []Mismatch{{Path: "p"}}},
		{Code: "C", Skipped: true},
		{Code: "D", Mismatches: []Mismatch{{Path: "q"}}},
	})
	assert.Equal(t, []string{"B", "D"}, out)
}

func TestRemediate_NoOpWhenNothingFailed(t *testing.T) {
	c := newFakeClientWith(t).Build()
	err := Remediate(context.Background(), Inputs{KubeClient: c}, []Result{
		{Code: CodeHelmValues, Mismatches: []Mismatch{{Path: "x"}}},      // not strays — no-op
		{Code: CodeStrayCRs},                                              // passed strays — no-op
	}, RemediationOptions{})
	require.NoError(t, err)
}

func TestRemediate_DeletesStrayCR(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}
	stray := newCR(gvk, "nvidia-network-operator", "old-net")
	c := newFakeClientWith(t, stray).Build()

	// Sanity check: object is present in the fake.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "nvidia-network-operator", Name: "old-net"}, got)
	require.NoError(t, err)

	results := []Result{{
		Code: CodeStrayCRs,
		Mismatches: []Mismatch{{
			Path:   "SriovNetwork/nvidia-network-operator/old-net",
			Detail: "sriovnetwork.openshift.io/v1/SriovNetwork; namespaced",
		}},
	}}
	err = Remediate(context.Background(), Inputs{KubeClient: c}, results, RemediationOptions{})
	require.NoError(t, err)

	// Object must now be absent.
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	err = c.Get(context.Background(), client.ObjectKey{Namespace: "nvidia-network-operator", Name: "old-net"}, got)
	require.Error(t, err)
}

func TestRemediate_DryRunDoesNotDelete(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}
	stray := newCR(gvk, "nvidia-network-operator", "old-net")
	c := newFakeClientWith(t, stray).Build()

	results := []Result{{
		Code: CodeStrayCRs,
		Mismatches: []Mismatch{{
			Path:   "SriovNetwork/nvidia-network-operator/old-net",
			Detail: "sriovnetwork.openshift.io/v1/SriovNetwork; namespaced",
		}},
	}}
	err := Remediate(context.Background(), Inputs{KubeClient: c}, results, RemediationOptions{DryRun: true})
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	err = c.Get(context.Background(), client.ObjectKey{Namespace: "nvidia-network-operator", Name: "old-net"}, got)
	require.NoError(t, err, "dry-run must not delete")
}

func TestRemediate_ToleratesAlreadyDeleted(t *testing.T) {
	// The stray was deleted between the check and remediate (race with
	// another caller). Should not surface as an error.
	c := newFakeClientWith(t).Build()
	results := []Result{{
		Code: CodeStrayCRs,
		Mismatches: []Mismatch{{
			Path:   "SriovNetwork/nvidia-network-operator/already-gone",
			Detail: "sriovnetwork.openshift.io/v1/SriovNetwork; namespaced",
		}},
	}}
	err := Remediate(context.Background(), Inputs{KubeClient: c}, results, RemediationOptions{})
	require.NoError(t, err)
}
