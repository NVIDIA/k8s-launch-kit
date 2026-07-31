// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newFakeClientWith builds a controller-runtime fake client seeded with the
// given unstructured objects. Each managed Kind needs a fake List type
// registered with the scheme for `client.List` to round-trip — otherwise
// the fake responds with "no kind registered".
func newFakeClientWith(t *testing.T, objs ...runtime.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, k := range managedKinds {
		listGVK := schema.GroupVersionKind{
			Group:   k.GVK.Group,
			Version: k.GVK.Version,
			Kind:    k.GVK.Kind + "List",
		}
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(listGVK)
		scheme.AddKnownTypeWithName(listGVK, ul)
		scheme.AddKnownTypeWithName(k.GVK, &unstructured.Unstructured{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func newCR(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	if namespace != "" {
		o.SetNamespace(namespace)
	}
	o.SetName(name)
	return o
}

func TestCheckStrayCRs_SkipsWithoutClient(t *testing.T) {
	r := CheckStrayCRs(context.Background(), Inputs{OperatorNamespace: "nvidia-network-operator"})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no kube client")
}

func TestCheckStrayCRs_SkipsWithoutNamespace(t *testing.T) {
	c := newFakeClientWith(t).Build()
	r := CheckStrayCRs(context.Background(), Inputs{KubeClient: c})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no operator namespace")
}

func TestCheckStrayCRs_EmptyClusterAllExpected(t *testing.T) {
	c := newFakeClientWith(t).Build()
	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        c,
		OperatorNamespace: "nvidia-network-operator",
	})
	require.False(t, r.Skipped)
	assert.Empty(t, r.Mismatches)
}

func TestCheckStrayCRs_FlagsClusterScopedStray(t *testing.T) {
	ncpGVK := schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"}
	stray := newCR(ncpGVK, "", "leftover-policy")
	c := newFakeClientWith(t, stray).Build()
	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        c,
		OperatorNamespace: "nvidia-network-operator",
		// Empty GeneratedManifests: we rendered nothing, so the
		// existing NCP is a stray.
	})
	require.False(t, r.Skipped)
	require.Len(t, r.Mismatches, 1)
	assert.Equal(t, "NicClusterPolicy/leftover-policy", r.Mismatches[0].Path)
	assert.Contains(t, r.Mismatches[0].Detail, "cluster-scoped")
}

func TestCheckStrayCRs_RecognisesExpected(t *testing.T) {
	ncpGVK := schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"}
	live := newCR(ncpGVK, "", "nic-cluster-policy")
	c := newFakeClientWith(t, live).Build()
	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        c,
		OperatorNamespace: "nvidia-network-operator",
		GeneratedManifests: []ObjectRef{
			{GVK: ncpGVK, Name: "nic-cluster-policy"},
		},
	})
	require.False(t, r.Skipped)
	assert.Empty(t, r.Mismatches)
}

func TestCheckStrayCRs_FlagsNamespacedStray(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}
	stray := newCR(gvk, "nvidia-network-operator", "old-net")
	expected := newCR(gvk, "nvidia-network-operator", "new-net")
	c := newFakeClientWith(t, stray, expected).Build()

	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        c,
		OperatorNamespace: "nvidia-network-operator",
		GeneratedManifests: []ObjectRef{
			{GVK: gvk, Namespace: "nvidia-network-operator", Name: "new-net"},
		},
	})
	require.False(t, r.Skipped)
	require.Len(t, r.Mismatches, 1)
	assert.Equal(t, "SriovNetwork/nvidia-network-operator/old-net", r.Mismatches[0].Path)
}

func TestCheckStrayCRs_IgnoresStrayInOtherNamespace(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}
	// A namespaced object in a DIFFERENT namespace — must not be
	// listed by the InNamespace(operatorNS) filter.
	stray := newCR(gvk, "some-other-ns", "stray-elsewhere")
	c := newFakeClientWith(t, stray).Build()
	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        c,
		OperatorNamespace: "nvidia-network-operator",
	})
	require.False(t, r.Skipped)
	assert.Empty(t, r.Mismatches)
}

func TestCheckStrayCRs_IgnoresSpectrumXOperatorGeneratedResources(t *testing.T) {
	namespace := "nvidia-network-operator"
	childKinds := []schema.GroupVersionKind{
		{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetworkNodePolicy"},
		{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetworkPoolConfig"},
		{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "OVSNetwork"},
	}
	objects := make([]runtime.Object, 0, len(childKinds)*2)
	for _, childGVK := range childKinds {
		operatorGenerated := newCR(childGVK, namespace, "rail0")
		operatorGenerated.SetLabels(map[string]string{spectrumXOwnerNameLabel: "rails"})
		objects = append(objects, operatorGenerated)

		// Preserve the existing protection for an unlabelled resource of the
		// same Kind: only Spectrum-X operator output is exempt.
		objects = append(objects, newCR(childGVK, namespace, "unmanaged"))
	}

	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        newFakeClientWith(t, objects...).Build(),
		OperatorNamespace: namespace,
	})
	require.False(t, r.Skipped)
	require.Len(t, r.Mismatches, len(childKinds))
	for _, mismatch := range r.Mismatches {
		assert.Contains(t, mismatch.Path, "/unmanaged")
	}
}

func TestCheckStrayCRs_DoesNotIgnoreOwnerLabelOnOtherKinds(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "sriovnetwork.openshift.io", Version: "v1", Kind: "SriovNetwork"}
	stray := newCR(gvk, "nvidia-network-operator", "rail0")
	stray.SetLabels(map[string]string{spectrumXOwnerNameLabel: "rails"})

	r := CheckStrayCRs(context.Background(), Inputs{
		KubeClient:        newFakeClientWith(t, stray).Build(),
		OperatorNamespace: "nvidia-network-operator",
	})
	require.Len(t, r.Mismatches, 1)
	assert.Equal(t, "SriovNetwork/nvidia-network-operator/rail0", r.Mismatches[0].Path)
}

func TestScanGeneratedManifests_FiltersValuesAndExamples(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("nfd:\n  enabled: true\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "10-nicclusterpolicy.yaml"), []byte(`apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
metadata:
  name: nic-cluster-policy
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "99-example-workload.yaml"), []byte(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: example
  namespace: default
`), 0o644))

	refs, err := ScanGeneratedManifests(dir)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "NicClusterPolicy", refs[0].GVK.Kind)
	assert.Equal(t, "nic-cluster-policy", refs[0].Name)
}

func TestScanGeneratedManifests_MultiDocFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ippools.yaml"), []byte(`apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
metadata:
  name: pool-0
  namespace: nvidia-network-operator
---
apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
metadata:
  name: pool-1
  namespace: nvidia-network-operator
`), 0o644))
	refs, err := ScanGeneratedManifests(dir)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "pool-0", refs[0].Name)
	assert.Equal(t, "pool-1", refs[1].Name)
}
