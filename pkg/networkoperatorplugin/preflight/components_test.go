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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newNcpWith(t *testing.T, name string, sections map[string]string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"})
	o.SetName(name)
	spec := map[string]interface{}{}
	for path, version := range sections {
		// path = dotted spec sub-tree e.g. "ofedDriver" or "nicConfigurationOperator.operator"
		set(spec, path, map[string]interface{}{"version": version})
	}
	require.NoError(t, unstructured.SetNestedMap(o.Object, spec, "spec"))
	return o
}

// set drops a nested map at a dotted path inside dst.
func set(dst map[string]interface{}, dotted string, value map[string]interface{}) {
	parts := splitDot(dotted)
	cur := dst
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return
		}
		next, ok := cur[p].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[p] = next
		}
		cur = next
	}
}

func splitDot(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// newFakeClientForNCP builds a fake client with the scheme set up for
// listing NicClusterPolicy + NicNodePolicy.
func newFakeClientForNCP(t *testing.T, objs ...runtime.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, kind := range []string{"NicClusterPolicy", "NicNodePolicy"} {
		gvk := schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind}
		listGVK := gvk
		listGVK.Kind = kind + "List"
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(listGVK)
		scheme.AddKnownTypeWithName(listGVK, ul)
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestCheckNCPComponentVersions_SkipsWithoutExpected(t *testing.T) {
	r := CheckNCPComponentVersions(context.Background(), Inputs{})
	require.True(t, r.Skipped)
	assert.Equal(t, CodeNCPComponentVersions, r.Code)
}

func TestCheckNCPComponentVersions_SkipsWithoutClient(t *testing.T) {
	r := CheckNCPComponentVersions(context.Background(), Inputs{
		ExpectedComponentVersion: "network-operator-v26.4.0",
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no kube client")
}

func TestCheckNCPComponentVersions_SkipsWhenNoNCP(t *testing.T) {
	c := newFakeClientForNCP(t).Build()
	r := CheckNCPComponentVersions(context.Background(), Inputs{
		KubeClient:               c,
		ExpectedComponentVersion: "network-operator-v26.4.0",
		ExpectedDOCAVersion:      "doca3.4.0",
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no NicClusterPolicy")
}

func TestCheckNCPComponentVersions_AllMatch(t *testing.T) {
	ncp := newNcpWith(t, "nic-cluster-policy", map[string]string{
		"ofedDriver":                              "doca3.4.0",
		"nicConfigurationOperator.operator":       "network-operator-v26.4.0",
		"nicConfigurationOperator.configurationDaemon": "network-operator-v26.4.0",
	})
	c := newFakeClientForNCP(t, ncp).Build()
	r := CheckNCPComponentVersions(context.Background(), Inputs{
		KubeClient:               c,
		ExpectedComponentVersion: "network-operator-v26.4.0",
		ExpectedDOCAVersion:      "doca3.4.0",
	})
	require.False(t, r.Skipped)
	assert.Empty(t, r.Mismatches)
}

func TestCheckNCPComponentVersions_ReportsDocaDriverDrift(t *testing.T) {
	ncp := newNcpWith(t, "nic-cluster-policy", map[string]string{
		"ofedDriver": "doca3.3.0-stale",
		"nvIpam":     "network-operator-v26.4.0",
	})
	c := newFakeClientForNCP(t, ncp).Build()
	r := CheckNCPComponentVersions(context.Background(), Inputs{
		KubeClient:               c,
		ExpectedComponentVersion: "network-operator-v26.4.0",
		ExpectedDOCAVersion:      "doca3.4.0",
	})
	require.False(t, r.Skipped)
	require.Len(t, r.Mismatches, 1)
	assert.Equal(t, "NicClusterPolicy/nic-cluster-policy.ofedDriver", r.Mismatches[0].Path)
	assert.Equal(t, "doca3.4.0", r.Mismatches[0].Expected)
	assert.Equal(t, "doca3.3.0-stale", r.Mismatches[0].Actual)
	assert.Equal(t, "docaDriver.version", r.Mismatches[0].Detail)
}

func TestCheckNCPComponentVersions_SkipsMissingSection(t *testing.T) {
	// nicConfigurationOperator not set at all — must not appear as a
	// mismatch ("section not rendered", not "wrong version").
	ncp := newNcpWith(t, "nic-cluster-policy", map[string]string{
		"ofedDriver": "doca3.4.0",
	})
	c := newFakeClientForNCP(t, ncp).Build()
	r := CheckNCPComponentVersions(context.Background(), Inputs{
		KubeClient:               c,
		ExpectedComponentVersion: "network-operator-v26.4.0",
		ExpectedDOCAVersion:      "doca3.4.0",
	})
	require.False(t, r.Skipped)
	assert.Empty(t, r.Mismatches)
}
