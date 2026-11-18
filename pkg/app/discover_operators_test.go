// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ocpUnstructured(apiVersion, kind, namespace, name string, extra map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
	for key, value := range extra {
		obj.Object[key] = value
	}
	return obj
}

func ocpPackageManifest(pkg, channel, source string) *unstructured.Unstructured {
	return ocpUnstructured("packages.operators.coreos.com/v1", "PackageManifest", "openshift-marketplace", pkg,
		map[string]interface{}{
			"status": map[string]interface{}{
				"packageName":            pkg,
				"defaultChannel":         channel,
				"catalogSource":          source,
				"catalogSourceNamespace": "openshift-marketplace",
			},
		})
}

const ocpFreshReferenceConfig = `flavor: ocp
networkOperator:
  namespace: nvidia-network-operator
  selectedRelease: "26.7"
docaDriver:
  enable: false
`

// ocpDiscoveryObjects returns the cluster objects discovery needs: an installed,
// release-26.7 network operator plus PackageManifests for every operator.
func ocpDiscoveryObjects(t *testing.T) []client.Object {
	t.Helper()
	sub := ocpUnstructured("operators.coreos.com/v1alpha1", "Subscription", "nvidia-network-operator", "nvidia-network-operator",
		map[string]interface{}{
			"spec":   map[string]interface{}{"name": "nvidia-network-operator"},
			"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0-1"},
		})
	csv := ocpUnstructured("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "nvidia-network-operator", "nvidia-network-operator.v26.7.0-1",
		map[string]interface{}{
			"spec":   map[string]interface{}{"version": "26.7.0-1"},
			"status": map[string]interface{}{"phase": "Succeeded"},
		})
	crd := ocpUnstructured("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "nicclusterpolicies.mellanox.com",
		map[string]interface{}{
			"spec":   map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v1alpha1", "served": true}}},
			"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}},
		})

	nwManifest := ocpPackageManifest("nvidia-network-operator", "stable", "certified-operators")
	require.NoError(t, unstructured.SetNestedSlice(nwManifest.Object, []interface{}{
		map[string]interface{}{"name": "v26.7", "currentCSVDesc": map[string]interface{}{"version": "26.7.1"}},
	}, "status", "channels"))

	return []client.Object{
		sub, csv, crd, nwManifest,
		ocpPackageManifest("sriov-network-operator", "stable", "redhat-operators"),
		ocpPackageManifest("nfd", "stable", "redhat-operators"),
		ocpPackageManifest("nvidia-maintenance-operator", "stable", "certified-operators"),
	}
}

func networkOperatorChannel(t *testing.T, cfg *config.LaunchKitConfig) string {
	t.Helper()
	require.NotNil(t, cfg.OCPOperatorInstall)
	for _, op := range cfg.OCPOperatorInstall.Operators {
		if op.Package == "nvidia-network-operator" {
			return op.Channel
		}
	}
	t.Fatal("nvidia-network-operator not in resolved install set")
	return ""
}

// Fresh discovery clears the profile before the hardware walk, so the operator
// set must be computed after it resolves: InfiniBand (→ "sriov") must yield
// sriov-network-operator in the saved ocpOperatorInstall block.
func TestDiscoverFreshOCPResolvesOperatorsAfterProfile(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "l8k-config.yaml"), []byte(ocpFreshReferenceConfig), 0o600))
	t.Chdir(tmpDir)
	outputConfig := filepath.Join(tmpDir, "cluster-config.yaml")

	launcher := newProfileDiscoveryLauncher(options.Options{
		SaveClusterConfig: outputConfig,
		ManageOperators:   true,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "InfiniBand"}})
	launcher.kubeClient = fake.NewClientBuilder().WithObjects(ocpDiscoveryObjects(t)...).Build()

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	require.Equal(t, "sriov", got.Profile.Deployment, "InfiniBand hardware resolves to an SR-IOV deployment")
	require.NotNil(t, got.OCPOperatorInstall)
	assert.True(t, got.OCPOperatorInstall.Manage)

	packages := make([]string, 0, len(got.OCPOperatorInstall.Operators))
	for _, op := range got.OCPOperatorInstall.Operators {
		packages = append(packages, op.Package)
	}
	sort.Strings(packages)
	assert.Equal(t, []string{
		"nfd",
		"nvidia-maintenance-operator",
		"nvidia-network-operator",
		"sriov-network-operator",
	}, packages, "the resolved SR-IOV profile must include sriov-network-operator")
}

// TestDiscoverOCPHonorsSavedManageSetting verifies discovery runs when
// ocpOperatorInstall.manage is set in config without the --manage-operators flag,
// so the saved channel matches the selected release instead of the offline default.
func TestDiscoverOCPHonorsSavedManageSetting(t *testing.T) {
	tmpDir := t.TempDir()
	ref := "flavor: ocp\n" +
		"networkOperator:\n  namespace: nvidia-network-operator\n  selectedRelease: \"26.7\"\n" +
		"ocpOperatorInstall:\n  manage: true\n" +
		"docaDriver:\n  enable: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "l8k-config.yaml"), []byte(ref), 0o600))
	t.Chdir(tmpDir)
	outputConfig := filepath.Join(tmpDir, "cluster-config.yaml")

	// Flag intentionally absent; manage comes from config only.
	launcher := newProfileDiscoveryLauncher(options.Options{
		SaveClusterConfig: outputConfig,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "InfiniBand"}})
	launcher.kubeClient = fake.NewClientBuilder().WithObjects(ocpDiscoveryObjects(t)...).Build()

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	assert.Equal(t, "v26.7", networkOperatorChannel(t, got), "discovery ran and pinned the release channel")
}
