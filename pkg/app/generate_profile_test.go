// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestGenerateDOCAHostDefaultReachesPoliciesAndEffectiveBundle(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-enable=%t", override), func(t *testing.T) {
			cfg, err := config.DefaultLaunchKitConfig()
			require.NoError(t, err)
			cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "host_device"}
			cfg.ClusterConfig = []config.ClusterConfig{{Identifier: "host-doca", WorkerNodes: []string{"worker-a"},
				NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-a"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}},
				PFs:          []config.PFConfig{{PciAddress: "0000:18:00.0", DeviceID: "1023", Traffic: "east-west"}}}}
			cfg.DiscoveredDOCAHost = []config.DOCAHostObservation{{Node: "worker-a", Status: config.DOCAHostInstalled, Version: "3.5.0-041000"}}
			raw, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			raw, err = omitDOCAHostDriverChoice(raw)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			output := filepath.Join(t.TempDir(), "deployment")
			opts := options.Options{SaveDeploymentFiles: output}
			if override {
				opts.EnableDocaDriver = &override
			}
			launcher := New(opts)
			launcher.ui = ui.NewSilent()
			launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}
			require.NoError(t, launcher.executeGeneration(path))
			artifacts, err := bundle.Load(os.DirFS(filepath.Join(output, networkoperatorplugin.PluginName)))
			require.NoError(t, err)
			policies := 0
			for _, document := range artifacts.Documents() {
				object := document.ObjectCopy()
				if object.GetKind() != "NicClusterPolicy" && object.GetKind() != "NicNodePolicy" {
					continue
				}
				policies++
				_, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "ofedDriver")
				require.NoError(t, err)
				assert.Equal(t, override, found, "host-device NCP must preserve the resolved driver ownership")
			}
			assert.Positive(t, policies)
			effective, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(output))
			require.NoError(t, err)
			assert.Equal(t, override, effective.DOCADriver.Enable)
			assert.Equal(t, cfg.DiscoveredDOCAHost, effective.DiscoveredDOCAHost)
		})
	}
}

func TestGeneratePresetOnlyOverridesTopologyAndRetainsHostEvidence(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))
	enabled, disabled := true, false
	for _, tc := range []struct {
		name, secondStatus, release, wantError string
		yamlChoice, cliChoice                  *bool
		noDiscovery, wantEnable, wantWarning   bool
		incompleteMembership                   bool
	}{
		{name: "installed default", wantEnable: false},
		{name: "mixed no choice", secondStatus: config.DOCAHostNotInstalled, wantError: "set docaDriver.enable explicitly"},
		{name: "mixed unknown no choice", secondStatus: config.DOCAHostUnknown, wantError: "set docaDriver.enable explicitly"},
		{name: "incomplete source membership", incompleteMembership: true, wantError: "lack complete workerNodes"},
		{name: "mixed manual true", secondStatus: config.DOCAHostNotInstalled, yamlChoice: &enabled, wantEnable: true},
		{name: "mixed manual false", secondStatus: config.DOCAHostNotInstalled, yamlChoice: &disabled},
		{name: "installed YAML true", yamlChoice: &enabled, wantEnable: true},
		{name: "installed YAML false", yamlChoice: &disabled},
		{name: "CLI true wins", yamlChoice: &disabled, cliChoice: &enabled, wantEnable: true},
		{name: "CLI false wins", yamlChoice: &enabled, cliChoice: &disabled},
		{name: "release mismatch", release: "26.10", wantWarning: true},
		{name: "preset only", noDiscovery: true, wantEnable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.DefaultLaunchKitConfig()
			require.NoError(t, err)
			cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "host_device"}
			cfg.NetworkOperator.SelectedRelease = "26.7"
			if tc.release != "" {
				cfg.NetworkOperator.SelectedRelease = tc.release
			}
			if !tc.noDiscovery {
				cfg.ClusterConfig = []config.ClusterConfig{
					{Identifier: "source-a", GPUType: "NVIDIA-H200", WorkerNodes: []string{"worker-a"}},
					{Identifier: "source-b", GPUType: "NVIDIA-H200", WorkerNodes: []string{"worker-b"}},
				}
				if tc.incompleteMembership {
					cfg.ClusterConfig[1].WorkerNodes = nil
				}
				second := config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostInstalled, Version: "3.5.0-1"}
				if tc.secondStatus != "" {
					second.Status, second.Version = tc.secondStatus, ""
				}
				cfg.DiscoveredDOCAHost = []config.DOCAHostObservation{
					{Node: "worker-a", Status: config.DOCAHostInstalled, Version: "3.5.0-1"}, second,
					{Node: "excluded", Status: config.DOCAHostInstalled, Version: "0.0.0-1"},
				}
			}
			if tc.yamlChoice != nil {
				cfg.DOCADriver.Enable = *tc.yamlChoice
			}
			raw, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			if tc.yamlChoice == nil {
				raw, err = omitDOCAHostDriverChoice(raw)
				require.NoError(t, err)
			}
			input := filepath.Join(t.TempDir(), "source.yaml")
			require.NoError(t, os.WriteFile(input, raw, 0o600))
			outputDir := filepath.Join(t.TempDir(), "deployment")
			launcher := New(options.Options{ForPreset: "GB300-DGX-Station-NVIDIA-GB300", NodeSelector: "role=preset-render",
				EnableDocaDriver: tc.cliChoice, SaveDeploymentFiles: outputDir})
			output := &testOutput{}
			launcher.ui = output
			launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}
			launcher.presetCatalog, err = presets.EmbeddedCatalog()
			require.NoError(t, err)
			err = launcher.executeGeneration(input)
			unchanged, readErr := os.ReadFile(input)
			require.NoError(t, readErr)
			assert.Equal(t, raw, unchanged, "preset generation must preserve the source config")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.NoDirExists(t, outputDir, "mixed ownership must fail before artifacts are written")
				return
			}
			require.NoError(t, err)
			got, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(outputDir))
			require.NoError(t, err)
			require.Len(t, got.ClusterConfig, 1)
			assert.Equal(t, "NVIDIA-GB300", got.ClusterConfig[0].GPUType)
			assert.Empty(t, got.ClusterConfig[0].WorkerNodes, "discovery workers must not become preset topology membership")
			assert.Equal(t, tc.wantEnable, got.DOCADriver.Enable)
			assert.Equal(t, cfg.DiscoveredDOCAHost, got.DiscoveredDOCAHost)
			if tc.noDiscovery {
				assert.Nil(t, got.DiscoveredDOCAHostScope)
			} else {
				require.NotNil(t, got.DiscoveredDOCAHostScope)
				assert.Equal(t, []string{"worker-a", "worker-b"}, got.DiscoveredDOCAHostScope.WorkerNodes)
				assert.True(t, got.DiscoveredDOCAHostScope.MembershipKnown)
			}
			warnings := strings.Join(output.warnings, "\n")
			assert.Equal(t, tc.wantWarning, strings.Contains(warnings, "release SBOM"))
			assert.NotContains(t, warnings, "excluded", "unselected stale observations must remain excluded")
			artifacts, err := bundle.Load(os.DirFS(filepath.Join(outputDir, networkoperatorplugin.PluginName)))
			require.NoError(t, err)
			ofeds, policies := 0, 0
			for _, document := range artifacts.Documents() {
				object := document.ObjectCopy()
				if object.GetKind() == "NicClusterPolicy" || object.GetKind() == "NicNodePolicy" {
					policies++
					_, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "ofedDriver")
					require.NoError(t, err)
					if found {
						ofeds++
					}
				}
			}
			assert.Positive(t, policies)
			assert.Equal(t, tc.wantEnable, ofeds > 0)

			// Reuse the resolved config as plain input with its driver choice
			// omitted: captured evidence must survive another topology override.
			repeated, err := yaml.Marshal(got)
			require.NoError(t, err)
			repeated, err = omitDOCAHostDriverChoice(repeated)
			require.NoError(t, err)
			repeatInput := filepath.Join(t.TempDir(), "reused.yaml")
			require.NoError(t, os.WriteFile(repeatInput, repeated, 0o600))
			for _, preset := range []string{"GB300-DGX-Station-NVIDIA-GB300", ""} {
				launcher.options.ForPreset = preset
				launcher.options.EnableDocaDriver = nil
				launcher.options.SaveDeploymentFiles = filepath.Join(t.TempDir(), "repeated")
				err = launcher.executeGeneration(repeatInput)
				if tc.secondStatus != "" {
					require.ErrorContains(t, err, "set docaDriver.enable explicitly")
				} else {
					require.NoError(t, err)
					reused, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(launcher.options.SaveDeploymentFiles))
					require.NoError(t, err)
					assert.Equal(t, got.DiscoveredDOCAHostScope, reused.DiscoveredDOCAHostScope)
					assert.Equal(t, tc.noDiscovery, reused.DOCADriver.Enable)
				}
			}
		})
	}
}

func TestExecuteGenerationPreservesStructuredTemplateValidationError(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.LoadFullConfig(
		filepath.Join("pkg", "networkoperatorplugin", "testdata", "grouping", "mixed-same-type.yaml"),
		ctrllog.Log,
	)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
	}
	cfg.ClusterConfig[0].NetplanManaged = true

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))

	launcher := New(options.Options{})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	err = launcher.executeGeneration(configPath)
	require.Error(t, err)
	var structured *apperrors.StructuredError
	require.True(t, errors.As(err, &structured))
	assert.Equal(t, apperrors.ExitValidation, structured.ExitCode)
	assert.Contains(t, structured.Message, "conflicting netplan configuration")
}

func TestGenerateLeavesSourceUntouchedAndWritesEffectiveConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.ClusterConfig = []config.ClusterConfig{{
		Identifier:   "test-group",
		LinkType:     "Ethernet",
		WorkerNodes:  []string{"worker-0"},
		NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-0"},
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{
			Sriov: true,
			Rdma:  true,
		}},
		PFs: []config.PFConfig{{
			DeviceID:         "1023",
			PciAddress:       "0000:05:00.0",
			RdmaDevice:       "mlx5_0",
			NetworkInterface: "net1",
			Traffic:          "east-west",
		}},
	}}
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "host_device"}
	cfg.NvIpam.ReserveFirstIPs = 2
	cfg.NvIpam.Subnets = []config.NvIpamSubnetConfig{{
		Subnet:  "192.168.50.0/24",
		Gateway: "192.168.50.1",
		Exclusions: []config.NvIpamExclusion{{
			StartIP: "192.168.50.20",
			EndIP:   "192.168.50.21",
		}},
	}}

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	source := "# original config comment\n" + string(raw)
	source = strings.Replace(source, "clusterConfig:\n", "clusterConfig: # hardware inventory comment\n", 1)
	source = strings.Replace(source, "profile:\n", "profile:\n  # profile settings comment\n", 1)
	source = strings.Replace(source, "linkType: Ethernet", "linkType: Ethernet # hardware detail comment", 1)

	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))
	outputDir := filepath.Join(t.TempDir(), "deployment")

	launcher := New(options.Options{
		DeploymentType:      "sriov",
		Multirail:           false,
		MultirailSet:        true,
		SaveDeploymentFiles: outputDir,
	})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	require.NoError(t, launcher.executeGeneration(configPath))

	updated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(updated), "generate must not rewrite user input")

	got, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(outputDir))
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "ethernet", got.Profile.Fabric)
	assert.Equal(t, "sriov", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "in-place write must preserve file permissions")

	require.NoError(t, launcher.executeGeneration(configPath))
	secondUpdate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(secondUpdate), "repeated generation must leave source YAML untouched")
}

func TestRA24UnknownPlatformGenerationPreservesSourceAndExistingOutput(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.NetworkOperator.SelectedRelease = "26.10"
	cfg.Profile = &config.Profile{
		Fabric: "ethernet", Deployment: "sriov", Multirail: true,
		SpectrumX: &config.ProfileSpectrumX{
			Enable: true, SPCXVersion: "RA2.4", MultiplaneMode: "none",
			NumberOfPlanes: 1, TopologyType: config.SpectrumXTopology2Tier,
		},
	}
	profileData, err := os.ReadFile(filepath.Join("pkg", "config", "testdata", "dospcx-configmap.yaml"))
	require.NoError(t, err)
	cfg.Profile.SpectrumX.Profile = string(profileData)
	require.NoError(t, config.NormalizeSpectrumXProfileConfig(cfg.Profile.SpectrumX))
	rail := 0
	cfg.ClusterConfig = []config.ClusterConfig{{
		Identifier: "unknown-platform", GPUType: "NVIDIA-Unknown-GPU", LinkType: "Ethernet",
		WorkerNodes:  []string{"worker-0"},
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}},
		PFs:          []config.PFConfig{{DeviceID: "1023", PciAddress: "0000:05:00.0", Traffic: "east-west", Rail: &rail}},
	}}
	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	source := append([]byte("# preserve this user configuration\n"), raw...)
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, source, 0o600))
	outputDir := filepath.Join(t.TempDir(), "deployment")
	pluginDir := filepath.Join(outputDir, networkoperatorplugin.PluginName)
	require.NoError(t, os.MkdirAll(pluginDir, 0o755))
	sentinel := filepath.Join(pluginDir, "previous-manifest.yaml")
	previous := []byte("previous generated contents\n")
	require.NoError(t, os.WriteFile(sentinel, previous, 0o600))

	launcher := New(options.Options{SaveDeploymentFiles: outputDir})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}
	err = launcher.executeGeneration(configPath)
	require.ErrorContains(t, err, "unresolved doSPCX platform")
	require.ErrorContains(t, err, `"unknown-platform"`)
	require.ErrorContains(t, err, `"NVIDIA-Unknown-GPU"`)
	require.Equal(t, source, mustRead(t, configPath))
	require.Equal(t, previous, mustRead(t, sentinel))
	entries, err := os.ReadDir(pluginDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failure must not replace or append generated manifests")
	_, err = os.Stat(config.EffectiveConfigPath(outputDir))
	require.True(t, os.IsNotExist(err), "failure must not write resolved bundle metadata")
	require.Empty(t, launcher.generatedBundles)
}

func TestResolveSpectrumXTopologyFile(t *testing.T) {
	t.Run("config relative path resolves from config directory", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "configs", "cluster-config.yaml")
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
				TopologyFile: "../topology.json",
			}},
		}

		resolveSpectrumXTopologyFile(configPath, cfg)

		assert.Equal(t,
			filepath.Join(filepath.Dir(configPath), "../topology.json"),
			cfg.Profile.SpectrumX.ResolvedTopologyFile)
	})

	t.Run("pre-resolved CLI path is preserved", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "configs", "cluster-config.yaml")
		resolvedCLIPath := filepath.Join(t.TempDir(), "topology.json")
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
				TopologyFile:         "topology.json",
				ResolvedTopologyFile: resolvedCLIPath,
			}},
		}

		resolveSpectrumXTopologyFile(configPath, cfg)

		assert.Equal(t, resolvedCLIPath, cfg.Profile.SpectrumX.ResolvedTopologyFile)
	})
}

func TestGenerateUsesPresetHardwareForDefaultsWithoutPersistingPresetInventory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	source := `networkOperator:
  selectedRelease: "26.4"
profile:
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    topologyType: 2-tier
clusterConfig:
  - identifier: source-inventory
    machineType: source-machine
    gpuType: NVIDIA-H200
    linkType: Ethernet
    pfs:
      - deviceID: a2dc
        traffic: east-west
`
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))

	catalog, err := presets.EmbeddedCatalog()
	require.NoError(t, err)
	launcher := New(options.Options{
		ForPreset:           "GB300-NVL-NVIDIA-GB300",
		NodeSelector:        "nvidia.com/gpu.product=NVIDIA-GB300",
		SaveDeploymentFiles: filepath.Join(t.TempDir(), "deployment"),
	})
	launcher.ui = ui.NewSilent()
	launcher.presetCatalog = catalog

	require.NoError(t, launcher.executeGeneration(configPath))

	unchanged, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(unchanged))

	got, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(launcher.options.SaveDeploymentFiles))
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	require.NotNil(t, got.Profile.SpectrumX)
	assert.Equal(t, "swplb", got.Profile.SpectrumX.MultiplaneMode)
	assert.Equal(t, 2, got.Profile.SpectrumX.NumberOfPlanes)
	require.Len(t, got.ClusterConfig, 1)
	assert.NotEqual(t, "source-inventory", got.ClusterConfig[0].Identifier)
	assert.Equal(t, "NVIDIA-GB300", got.ClusterConfig[0].GPUType)
}

func TestExecuteGenerationRejectsDuplicateClusterConfigIdentifiers(t *testing.T) {
	// Two groups sharing an identifier must be rejected in the actual
	// generation path (issue #234): the per-source output filenames are
	// derived from the identifier, so the second group would silently
	// overwrite the first and only its NICs would be configured.
	source := `networkOperator:
  version: v25.10.0
  componentVersion: network-operator-v25.10.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
clusterConfig:
  - identifier: dgx-h100
    machineType: dgx-h100
    gpuType: NVIDIA-H100
    linkType: Ethernet
    workerNodes:
      - node-a
    pfs:
      - deviceID: a2dc
        traffic: east-west
  - identifier: dgx-h100
    machineType: dgx-h100
    gpuType: NVIDIA-H100
    linkType: Ethernet
    workerNodes:
      - node-b
    pfs:
      - deviceID: a2dc
        traffic: east-west
`
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))

	outputDir := filepath.Join(t.TempDir(), "manifests")
	launcher := New(options.Options{SaveDeploymentFiles: outputDir})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	err := launcher.executeGeneration(configPath)
	require.Error(t, err)
	var structured *apperrors.StructuredError
	require.True(t, errors.As(err, &structured))
	assert.Equal(t, apperrors.ExitValidation, structured.ExitCode)
	assert.Contains(t, structured.Message, `duplicate identifier "dgx-h100"`)

	// Generation must fail before producing any manifests.
	_, statErr := os.Stat(outputDir)
	assert.True(t, os.IsNotExist(statErr), "no deployment files should be written when validation fails")
}

func TestGenerateIPoIBInboxAdvisoryWorksOfflineAndPreservesCapabilityGate(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))
	for _, preset := range []bool{false, true} {
		t.Run(fmt.Sprintf("preset-%t", preset), func(t *testing.T) {
			cfg, err := config.DefaultLaunchKitConfig()
			require.NoError(t, err)
			cfg.Profile = &config.Profile{Fabric: "infiniband", Deployment: "rdma_shared"}
			cfg.DOCADriver.Enable = false
			cfg.ClusterConfig = []config.ClusterConfig{{Identifier: "inbox", WorkerNodes: []string{"worker-0"}, NodeSelector: map[string]string{"group": "inbox"}, Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Ib: true, Rdma: true, Sriov: true}}, PFs: []config.PFConfig{{PciAddress: "0000:18:00.0", DeviceID: "1023", Traffic: "east-west"}}}}
			raw, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "cluster-config.yaml")
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			opts := options.Options{SaveDeploymentFiles: filepath.Join(t.TempDir(), "output")}
			if preset {
				opts.ForPreset = "GB300-DGX-Station-NVIDIA-GB300"
				opts.NodeSelector = "group=inbox"
			}
			launcher := New(opts)
			launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}
			launcher.presetCatalog, err = presets.EmbeddedCatalog()
			require.NoError(t, err)
			out := &testOutput{}
			launcher.ui = out
			require.NoError(t, launcher.executeGeneration(path))
			assert.Len(t, out.warnings, 1)
			assert.Contains(t, out.warnings[0], "IPoIB")
			// A file-backed invalid capability must still fail profile selection before
			// emitting a host-readiness advisory; #303 remains authoritative.
			if !preset {
				cfg.ClusterConfig[0].Capabilities.Nodes.Ib = false
				raw, err = yaml.Marshal(cfg)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, raw, 0o600))
				out.warnings = nil
				err = launcher.executeGeneration(path)
				require.Error(t, err)
				assert.Empty(t, out.warnings)
			}
		})
	}
}
