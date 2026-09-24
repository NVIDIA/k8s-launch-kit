// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

func TestResolvePrecedenceAndInputImmutability(t *testing.T) {
	input, err := config.DecodeInput([]byte(`networkOperator:
  namespace: from-yaml
profile:
  fabric: infiniband
  multirail: false
docaDriver:
  enable: false
clusterConfig:
  - identifier: group-a
    linkType: InfiniBand
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`networkOperator:
  namespace: from-defaults
networkNamespaces: [default]
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
docaDriver:
  enable: true
clusterConfig:
  - identifier: default-hardware-must-not-leak
`), "defaults.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{
		Input:    input,
		Defaults: defaults,
		Options: options.Options{ConfigInputs: configinput.Values{Overrides: []configinput.Override{
			{Flag: "fabric", Path: "profile.fabric", Value: "ethernet"},
			{Flag: "network-namespaces", Path: "networkNamespaces", Value: []string{}},
		}}},
		ApplyHardwareDefaults: true,
		ValidateReady:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, "ethernet", result.Config.Profile.Fabric)
	assert.Equal(t, "sriov", result.Config.Profile.Deployment)
	assert.False(t, result.Config.Profile.Multirail, "explicit YAML false must survive")
	assert.False(t, result.Config.DOCADriver.Enable, "explicit YAML false must survive")
	assert.Equal(t, "from-yaml", result.Config.NetworkOperator.Namespace)
	assert.Empty(t, result.Config.NetworkNamespaces, "explicit CLI empty list must survive")
	require.Len(t, result.Config.ClusterConfig, 1)
	assert.Equal(t, "group-a", result.Config.ClusterConfig[0].Identifier)
	assert.Equal(t, "infiniband", input.Config.Profile.Fabric, "input must remain unchanged")
}

func TestResolveFlavorCLIOverridesYAMLAndAppliesOperatorDefaults(t *testing.T) {
	input, err := config.DecodeInput([]byte(`flavor: k8s
sriov:
  operatorNamespace: custom-sriov
nfd:
  configurationName: custom-nfd
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte("{}"), "defaults.yaml")
	require.NoError(t, err)
	result, err := Resolve(Request{Input: input, Defaults: defaults, Options: options.Options{Flavor: config.FlavorOCP}})
	require.NoError(t, err)
	require.Equal(t, config.FlavorOCP, result.Config.Flavor)
	require.Equal(t, "custom-sriov", result.Config.Sriov.OperatorNamespace)
	require.Equal(t, "custom-nfd", result.Config.NFD.ConfigurationName)
	require.Equal(t, config.DefaultNFDOperatorNamespace, result.Config.NFD.OperatorNamespace)
	require.Equal(t, config.DefaultMaintenanceOperatorNamespace, result.Config.Maintenance.OperatorNamespace)
	require.Equal(t, config.FlavorK8s, input.Config.Flavor, "resolver must not mutate input")

	k8s, err := Resolve(Request{Input: input, Defaults: defaults, Options: options.Options{Flavor: config.FlavorK8s}})
	require.NoError(t, err)
	require.Equal(t, config.FlavorK8s, k8s.Config.Flavor)
	require.NotNil(t, k8s.Config.NFD)
	require.Equal(t, "custom-nfd", k8s.Config.NFD.ConfigurationName)
	require.Empty(t, k8s.Config.NFD.OperatorNamespace, "OpenShift-only defaults must not leak into Kubernetes")

	_, err = Resolve(Request{Input: input, Defaults: defaults, Options: options.Options{Flavor: "unknown"}})
	require.ErrorContains(t, err, "flavor must be")
}

func TestResolveDefaultsDoNotShareNestedCollections(t *testing.T) {
	input, err := config.DecodeInput([]byte("{}"), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`nvIpam:
  subnets:
    - subnet: 192.168.0.0/24
      exclusions:
        - startIP: 192.168.0.2
          endIP: 192.168.0.3
`), "defaults.yaml")
	require.NoError(t, err)
	first, err := Resolve(Request{Input: input, Defaults: defaults})
	require.NoError(t, err)
	second, err := Resolve(Request{Input: input, Defaults: defaults})
	require.NoError(t, err)
	first.Config.NvIpam.Subnets[0].Exclusions[0].StartIP = "192.168.0.1"
	assert.Equal(t, "192.168.0.2", defaults.Config.NvIpam.Subnets[0].Exclusions[0].StartIP)
	assert.Equal(t, "192.168.0.2", second.Config.NvIpam.Subnets[0].Exclusions[0].StartIP)
}

func TestResolveCatalogSelectionOverridesStaleCoordinates(t *testing.T) {
	input, err := config.DecodeInput([]byte(`networkOperator:
  selectedRelease: "26.4"
  version: stale
  componentVersion: stale
  repository: stale
docaDriver:
  version: stale
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`{}`), "defaults.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{Input: input, Defaults: defaults})
	require.NoError(t, err)
	assert.Equal(t, "26.4", result.Config.NetworkOperator.SelectedRelease)
	assert.NotEqual(t, "stale", result.Config.NetworkOperator.Version)
	assert.NotEqual(t, "stale", result.Config.DOCADriver.Version)
}

func TestResolveCombinesSpectrumXCLIFieldsWithYAMLEnablement(t *testing.T) {
	input, err := config.DecodeInput([]byte(`networkOperator:
  selectedRelease: "26.4"
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    multiplaneMode: swplb
    numberOfPlanes: 2
    topologyType: 2-tier
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`{}`), "defaults.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{
		Input:    input,
		Defaults: defaults,
		Options: options.Options{ConfigInputs: configinput.Values{Overrides: []configinput.Override{
			{Flag: "number-of-planes", Path: "profile.spectrumX.numberOfPlanes", Value: 4},
		}}},
		ValidateReady: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 4, result.Config.Profile.SpectrumX.NumberOfPlanes)
}

func TestResolveRejectsOrphanedSpectrumXCLIFieldsAfterMerge(t *testing.T) {
	input, err := config.DecodeInput([]byte(`profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`{}`), "defaults.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{
		Input:    input,
		Defaults: defaults,
		Options: options.Options{ConfigInputs: configinput.Values{Overrides: []configinput.Override{
			{Flag: "number-of-planes", Path: "profile.spectrumX.numberOfPlanes", Value: 4},
		}}},
		ValidateReady: true,
	})
	require.ErrorContains(t, err, "numberOfPlanes is set but spectrumX.enable=false")
}

func TestResolvePreservesExplicitEmptyAndZeroAcrossHardwareDefaults(t *testing.T) {
	input, err := config.DecodeInput([]byte(`profile:
  fabric: ""
  deployment: ""
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    multiplaneMode: swplb
    numberOfPlanes: 0
clusterConfig:
  - identifier: group-a
    linkType: InfiniBand
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`profile:
  fabric: ethernet
  deployment: sriov
  spectrumX:
    numberOfPlanes: 4
`), "defaults.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{
		Input:                 input,
		Defaults:              defaults,
		ApplyHardwareDefaults: true,
	})
	require.NoError(t, err)

	assert.Empty(t, result.Config.Profile.Fabric)
	assert.Empty(t, result.Config.Profile.Deployment)
	assert.Zero(t, result.Config.Profile.SpectrumX.NumberOfPlanes)
}

func TestResolvePreservesScalarAliasFalseAndMaintenanceZero(t *testing.T) {
	input, err := config.DecodeInput([]byte(`profile:
  ignoreARP: &disabled false
docaDriver:
  enable: *disabled
maintenance:
  maxUnavailable: 0
`), "user.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{Input: input})
	require.NoError(t, err)
	assert.False(t, result.Config.DOCADriver.Enable)
	assert.Equal(t, "0", result.Config.Maintenance.MaxUnavailable.String())
}

func TestResolveRejectsExplicitInvalidValidationValue(t *testing.T) {
	input, err := config.DecodeInput([]byte(`validation:
  rdma:
    rpingIterations: -1
`), "user.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{Input: input})
	require.ErrorContains(t, err, "rpingIterations")
}

func TestResolveRejectsExplicitZeroNvIpamBlockSize(t *testing.T) {
	input, err := config.DecodeInput([]byte(`nvIpam:
  perNodeBlockSize: 0
`), "user.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{Input: input})
	require.ErrorContains(t, err, "perNodeBlockSize must be > 0")
}

func TestResolveRejectsExplicitEmptyValidationScalars(t *testing.T) {
	input, err := config.DecodeInput([]byte(`validation:
  mode: ""
  gpuDirect:
    gpuResourceType: ""
`), "user.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{Input: input})
	require.ErrorContains(t, err, "validation.mode must be one of")
}

func TestResolvePlacesHardwareDefaultsAboveCanonicalDefaults(t *testing.T) {
	input, err := config.DecodeInput([]byte(`clusterConfig:
  - identifier: group-a
    linkType: InfiniBand
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`profile:
  fabric: ethernet
`), "defaults.yaml")
	require.NoError(t, err)

	result, err := Resolve(Request{
		Input:                 input,
		Defaults:              defaults,
		ApplyHardwareDefaults: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "infiniband", result.Config.Profile.Fabric)
}

func TestResolveRejectsExplicitEmptyRelease(t *testing.T) {
	input, err := config.DecodeInput([]byte(`{}`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`{}`), "defaults.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{
		Input:    input,
		Defaults: defaults,
		Options: options.Options{ConfigInputs: configinput.Values{Requests: []configinput.Request{
			{Flag: "network-operator-release", Kind: "network-operator-release", Value: ""},
		}}},
	})
	require.ErrorContains(t, err, "release must not be empty")
}

func TestResolveRejectsOrphanedSpectrumXConfigRequest(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "profile.yaml")
	require.NoError(t, os.WriteFile(profilePath, []byte("useSoftwareCCAlgorithm: true\n"), 0o600))
	input, err := config.DecodeInput([]byte(`profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
`), "user.yaml")
	require.NoError(t, err)
	defaults, err := config.DecodeInput([]byte(`{}`), "defaults.yaml")
	require.NoError(t, err)

	_, err = Resolve(Request{
		Input:    input,
		Defaults: defaults,
		Options: options.Options{ConfigInputs: configinput.Values{Requests: []configinput.Request{
			{Flag: "spectrum-x-config", Kind: "spectrum-x-config", Value: profilePath},
		}}},
		ValidateReady: true,
	})
	require.ErrorContains(t, err, "profile content is set but spectrumX.enable=false")
}

func TestResolveReleaseSelectionWithCanonicalDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		opts options.Options
		want string
	}{
		{name: "ordinary default remains 26.7", yaml: "", opts: options.Options{}, want: "26.7"},
		{name: "user release overrides canonical", yaml: "networkOperator: {selectedRelease: '26.4'}\n", opts: options.Options{}, want: "26.4"},
		{name: "CLI release overrides user", yaml: "networkOperator: {selectedRelease: '26.4'}\n", opts: options.Options{NetworkOperatorRelease: "26.1"}, want: "26.1"},
		{name: "Spectrum-X selects compatible older release", yaml: "", opts: options.Options{
			SpectrumX: true, SPCXVersion: "RA2.2", MultiplaneMode: "none", TopologyScheme: config.SpectrumXTopology2Tier,
		}, want: "26.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := config.DecodeInput([]byte(tc.yaml+"profile: {fabric: ethernet}\n"), "user.yaml")
			require.NoError(t, err)
			result, err := Resolve(Request{Input: input, Options: tc.opts, ApplyHardwareDefaults: true, ValidateReady: true})
			require.NoError(t, err)
			catalog, ok := releases.LookupRelease(tc.want)
			require.True(t, ok)
			assert.Equal(t, tc.want, result.Config.NetworkOperator.SelectedRelease)
			assert.Equal(t, catalog.NetworkOperator.Version, result.Config.NetworkOperator.Version)
			assert.Equal(t, catalog.NetworkOperator.ComponentVersion, result.Config.NetworkOperator.ComponentVersion)
			assert.Equal(t, catalog.DOCADriver.Version, result.Config.DOCADriver.Version)
		})
	}
}
