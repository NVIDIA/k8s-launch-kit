// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
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

// ocpDiscoveryObjects models a partially installed cluster with catalog entries for every operator.
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

func TestDiscoverOCPWithoutOperatorsThenGenerateOffline(t *testing.T) {
	_, name, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Join(filepath.Dir(name), "..", ".."))

	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.yaml")
	inventory := filepath.Join(dir, "hardware.yaml")
	output := filepath.Join(dir, "deployment")
	require.NoError(t, os.WriteFile(seed, []byte("# site settings\n"+ocpFreshReferenceConfig), 0600))
	workers := []config.ClusterConfig{{
		Identifier: "worker", LinkType: "Ethernet", WorkerNodes: []string{"worker-a"},
		NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-a"},
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}},
		PFs:          []config.PFConfig{{PciAddress: "0000:18:00.0", DeviceID: "1023", RdmaDevice: "mlx5_0", NetworkInterface: "net1", Traffic: "east-west"}},
	}}
	discover := newProfileDiscoveryLauncher(options.Options{UserConfig: seed, SaveClusterConfig: inventory}, workers)
	var messages bytes.Buffer
	discover.ui = ui.NewJSON(io.Discard, &messages)
	discover.kubeClient = fake.NewClientBuilder().Build()
	require.NoError(t, discover.discoverClusterConfig())
	saved, err := config.LoadInput(inventory, discover.logger)
	require.NoError(t, err)
	require.Len(t, saved.Config.DiscoveredOCPOperators, 4)
	for _, op := range saved.Config.DiscoveredOCPOperators {
		require.NotEmpty(t, op.Issue)
		assert.Contains(t, messages.String(), op.Package)
	}
	assert.False(t, saved.Present.Has("profile.deployment"), "discover preserves fields omitted from a user seed")
	assert.Contains(t, string(saved.SourceYAML), "# site settings")

	generate := New(options.Options{SaveDeploymentFiles: output})
	generate.ui = ui.NewSilent()
	generate.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}
	require.NoError(t, os.Mkdir(output, 0755))
	sentinel := filepath.Join(output, "existing-file")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0600))
	err = generate.executeGeneration(inventory)
	var validation *apperrors.StructuredError
	require.ErrorAs(t, err, &validation)
	assert.Contains(t, validation.Suggestion, "--manage-operators")
	for _, op := range saved.Config.DiscoveredOCPOperators {
		assert.ErrorContains(t, err, op.Package)
	}
	assert.FileExists(t, sentinel)
	assert.NoDirExists(t, filepath.Join(output, networkoperatorplugin.PluginName))

	generate.options.ManageOperators = true
	require.NoError(t, generate.executeGeneration(inventory))
	assert.FileExists(t, filepath.Join(output, networkoperatorplugin.PluginName, "00-subscriptions.yaml"))
	// Refresh the same inventory after the operators have been installed.
	// CSV-only installs are valid input to the live readiness check.
	ready := ocpDiscoveryObjects(t)
	for _, op := range []struct{ pkg, ns, crd string }{
		{"sriov-network-operator", config.DefaultSriovOperatorNamespace, "sriovoperatorconfigs.sriovnetwork.openshift.io"},
		{"nfd", config.DefaultNFDOperatorNamespace, "nodefeaturediscoveries.nfd.openshift.io"},
		{"nvidia-maintenance-operator", config.DefaultMaintenanceOperatorNamespace, "maintenanceoperatorconfigs.maintenance.nvidia.com"},
	} {
		csv := ocpUnstructured("operators.coreos.com/v1alpha1", "ClusterServiceVersion", op.ns, op.pkg+".v1.0.0", map[string]interface{}{
			"status": map[string]interface{}{"phase": "Succeeded"},
		})
		crd := ocpUnstructured("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", op.crd, map[string]interface{}{
			"spec":   map[string]interface{}{"versions": []interface{}{map[string]interface{}{"served": true}}},
			"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}},
		})
		ready = append(ready, csv, crd)
	}
	discover.options.UserConfig = inventory
	discover.kubeClient = fake.NewClientBuilder().WithObjects(ready...).Build()
	require.NoError(t, discover.discoverClusterConfig())
	refreshed, err := config.LoadInput(inventory, discover.logger)
	require.NoError(t, err)
	require.Len(t, refreshed.Config.DiscoveredOCPOperators, 4)
	for _, op := range refreshed.Config.DiscoveredOCPOperators {
		assert.Empty(t, op.Issue)
	}
	generate.options.ManageOperators = false
	generate.options.SaveDeploymentFiles = filepath.Join(dir, "unmanaged")
	require.NoError(t, generate.executeGeneration(inventory))
	manifests := filepath.Join(generate.options.SaveDeploymentFiles, networkoperatorplugin.PluginName)
	assert.NoFileExists(t, filepath.Join(manifests, "00-subscriptions.yaml"))
	assert.DirExists(t, manifests)

}

func TestUnmanagedDiscoveryDefersOperatorGroupPlanningError(t *testing.T) {
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, Profile: &config.Profile{Deployment: "sriov"}}
	selectorGroup := ocpUnstructured("operators.coreos.com/v1", "OperatorGroup", config.DefaultNFDOperatorNamespace, "site-group",
		map[string]interface{}{"spec": map[string]interface{}{"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"team": "networking"}}}})
	launcher := newProfileDiscoveryLauncher(options.Options{}, nil)
	launcher.kubeClient = fake.NewClientBuilder().WithObjects(selectorGroup).Build()
	require.NoError(t, launcher.discoverOCPOperators(t.Context(), cfg))
	require.Len(t, cfg.DiscoveredOCPOperators, 4)
	require.NotNil(t, cfg.OCPOperatorInstall)
	require.Contains(t, cfg.OCPOperatorInstall.DiscoveryError, "selector-based")

	path := filepath.Join(t.TempDir(), "inventory.yaml")
	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	launcher.options.ManageOperators = true
	err = launcher.executeGeneration(path)
	require.ErrorContains(t, err, "selector-based")

	launcher.kubeClient = fake.NewClientBuilder().Build()
	require.NoError(t, launcher.discoverOCPOperators(t.Context(), cfg))
	if cfg.OCPOperatorInstall != nil {
		require.Empty(t, cfg.OCPOperatorInstall.DiscoveryError)
	}
}
