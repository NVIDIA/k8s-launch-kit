// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applySpectrumXDefaults is a test-only shim that runs the full
// validation + defaulting pipeline against an Options struct. The
// production split is:
//
//	pkg/cmd.applySpectrumXSyntaxChecks  → Phase 1 (PreRunE)
//	pkg/resolve.ApplyHardwareDefaults    → fills empty cfg fields
//	plugin.ApplyOptionsToConfig          → CLI overlay
//	pkg/resolve.ValidateResolvedConfig   → Phase 2 cohort
//
// This helper chains those four steps against a synthesized cfg so
// pre-Unit-8 tests (which expected a single combined `applySpectrumXDefaults(opts)`)
// keep asserting the same observable behaviour. After the chain
// completes, the resolved cfg's profile fields are reflected back onto
// opts so legacy assertions like `assert.Equal(t, "ethernet", opts.Fabric)`
// still see the implicit Spectrum-X → ethernet default.
func applySpectrumXDefaults(opts *options.Options) error {
	if err := applySpectrumXSyntaxChecks(opts); err != nil {
		return err
	}
	cfg := &config.LaunchKubernetesConfig{
		Profile: &config.Profile{
			Fabric:     opts.Fabric,
			Deployment: opts.DeploymentType,
			Multirail:  opts.Multirail,
		},
	}
	if opts.SpectrumX {
		cfg.Profile.SpectrumX = &config.ProfileSpectrumX{
			Enable:         true,
			SPCXVersion:    opts.SPCXVersion,
			MultiplaneMode: opts.MultiplaneMode,
			NumberOfPlanes: opts.NumberOfPlanes,
		}
	}
	if opts.NetworkOperatorRelease != "" {
		cfg.NetworkOperator = &config.NetworkOperatorConfig{
			SelectedRelease: opts.NetworkOperatorRelease,
		}
	}
	resolve.ApplyHardwareDefaults(cfg, *opts)
	if err := resolve.ValidateResolvedConfig(cfg); err != nil {
		return err
	}
	// Reflect filled-in fields back on opts.
	opts.Fabric = cfg.Profile.Fabric
	opts.DeploymentType = cfg.Profile.Deployment
	opts.Multirail = cfg.Profile.Multirail
	if cfg.NetworkOperator != nil {
		opts.NetworkOperatorRelease = cfg.NetworkOperator.SelectedRelease
	}
	return nil
}

// minSpectrumXOpts returns a minimally valid Spectrum-X options struct: all
// cohort flags satisfied, value-validated, paired correctly with the
// network-operator release. Tests that exercise the Spectrum-X
// defaults/expansion logic should start from this baseline so a tightening
// of the cohort validation only forces updates here.
func minSpectrumXOpts() *options.Options {
	return &options.Options{
		SpectrumX:              true,
		SPCXVersion:            "RA2.2",
		MultiplaneMode:         "hwplb",
		NumberOfPlanes:         4,
		NetworkOperatorRelease: "26.4",
	}
}

func TestApplySpectrumXDefaults_SetsImpliedValues(t *testing.T) {
	opts := minSpectrumXOpts()
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Equal(t, "ethernet", opts.Fabric)
	assert.Equal(t, "sriov", opts.DeploymentType)
	assert.True(t, opts.Multirail)
}

func TestApplySpectrumXDefaults_RequiresVersionWhenSpectrumXSet(t *testing.T) {
	// SpectrumX was set but the RA version is empty — defensive path.
	// Run() normally derives SpectrumX from SPCXVersion != "", so this
	// branch only fires if a caller bypasses that derivation.
	opts := minSpectrumXOpts()
	opts.SPCXVersion = ""
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--spectrum-x requires the SPC-X RA version")
}

func TestApplySpectrumXDefaults_RejectsInvalidVersion(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.SPCXVersion = "2.1" // common typo: missing the "RA" prefix
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid --spectrum-x value "2.1"`)
}

func TestApplySpectrumXDefaults_RequiresMultiplaneMode(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.MultiplaneMode = ""
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--multiplane-mode is required")
}

func TestApplySpectrumXDefaults_RejectsInvalidMultiplaneMode(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.MultiplaneMode = "bogus"
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid --multiplane-mode "bogus"`)
}

func TestApplySpectrumXDefaults_RequiresNumberOfPlanes(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.NumberOfPlanes = 0
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--number-of-planes is required")
}

func TestApplySpectrumXDefaults_RejectsInvalidNumberOfPlanes(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.NumberOfPlanes = 3
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --number-of-planes 3")
}

func TestApplySpectrumXDefaults_RejectsNoneWithMultiplePlanes(t *testing.T) {
	// "none" disables multiplaning, so >1 planes makes no physical sense.
	for _, n := range []int{2, 4} {
		opts := minSpectrumXOpts()
		opts.MultiplaneMode = "none"
		opts.NumberOfPlanes = n
		err := applySpectrumXDefaults(opts)
		require.Error(t, err, "planes=%d should be rejected with mode=none", n)
		assert.Contains(t, err.Error(), "--multiplane-mode none requires --number-of-planes 1")
	}
}

func TestApplySpectrumXDefaults_AcceptsNoneWithSinglePlane(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.MultiplaneMode = "none"
	opts.NumberOfPlanes = 1
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err, "mode=none with planes=1 must be accepted")
}

func TestApplySpectrumXDefaults_DefaultsNetworkOperatorReleaseFromRA(t *testing.T) {
	// Unit 8: when --spectrum-x is set without --network-operator-release,
	// the release auto-defaults from the canonical (RA → release) map in
	// `config.SPCXVersionAllowedReleases`. The user no longer has to pass
	// the release explicitly.
	for ra, release := range map[string]string{"RA2.1": "26.1", "RA2.2": "26.4"} {
		t.Run(ra, func(t *testing.T) {
			opts := minSpectrumXOpts()
			opts.SPCXVersion = ra
			opts.NetworkOperatorRelease = ""
			err := applySpectrumXDefaults(opts)
			require.NoError(t, err, "release should auto-default for %s", ra)
			assert.Equal(t, release, opts.NetworkOperatorRelease,
				"release should default to %s for %s", release, ra)
		})
	}
}

func TestApplySpectrumXDefaults_RejectsRAReleaseMismatch(t *testing.T) {
	// RA2.1 + 26.4 must produce a specific pair-mismatch error rather than
	// falling through to "no applicable profile found".
	opts := minSpectrumXOpts()
	opts.SPCXVersion = "RA2.1"
	opts.NetworkOperatorRelease = "26.4"
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--spectrum-x RA2.1 requires --network-operator-release")
	assert.Contains(t, err.Error(), "26.1")
}

func TestApplySpectrumXDefaults_RejectsCohortFlagsWithoutSpectrumX(t *testing.T) {
	cases := map[string]*options.Options{
		"multiplane-mode":  {MultiplaneMode: "hwplb"},
		"number-of-planes": {NumberOfPlanes: 4},
	}
	for flag, opts := range cases {
		t.Run(flag, func(t *testing.T) {
			err := applySpectrumXDefaults(opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+flag+" can only be used with --spectrum-x")
		})
	}
}

func TestApplySpectrumXDefaults_AppliesGenericDefaultsWhenSpectrumXDisabled(t *testing.T) {
	// Unit 8: even without --spectrum-x, ApplyHardwareDefaults sets the
	// always-on defaults (Deployment=sriov, Multirail=true). Fabric is
	// only filled when cluster hardware confirms a unanimous linkType
	// (skipped here — synthetic cfg has no clusterConfig).
	opts := &options.Options{
		SpectrumX: false,
	}
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Empty(t, opts.Fabric, "fabric stays empty without cluster linkType")
	assert.Equal(t, "sriov", opts.DeploymentType, "deployment-type defaults to sriov")
	assert.True(t, opts.Multirail, "multirail defaults to true")
}

func TestApplySpectrumXDefaults_AcceptsMatchingFabric(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.Fabric = "ethernet"
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Equal(t, "ethernet", opts.Fabric)
}

func TestApplySpectrumXDefaults_ErrorOnConflictingFabric(t *testing.T) {
	// Cohort error: --spectrum-x with explicit --fabric=infiniband
	// must reject. Phase 2's `validateSpectrumXCohort` catches it
	// against the resolved cfg.
	opts := minSpectrumXOpts()
	opts.Fabric = "infiniband"
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ethernet")
	assert.Contains(t, err.Error(), "infiniband")
}

func TestApplySpectrumXDefaults_ErrorOnConflictingDeploymentType(t *testing.T) {
	opts := minSpectrumXOpts()
	opts.DeploymentType = "host_device"
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sriov")
	assert.Contains(t, err.Error(), "host_device")
}

func TestValidateConfig_DeployWithUserConfigPassesWithoutProfileFlags(t *testing.T) {
	opts := options.Options{
		UserConfig:     "some-config.yaml",
		Deploy:         true,
		Kubeconfig:     "/path/to/kubeconfig",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
	}
	err := validateConfig(&opts)
	assert.NoError(t, err)
}

func TestValidateConfig_DeployWithoutAnyProfileSourceFails(t *testing.T) {
	opts := options.Options{
		DiscoverClusterConfig: true,
		Deploy:                true,
		Kubeconfig:            "/path/to/kubeconfig",
		EnabledPlugins:        []string{"network-operator"},
		OutputFormat:          "text",
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--deploy requires")
}

func TestValidateConfig_InvalidOutputFormat(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "xml",
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--output must be one of")
}

func TestValidateConfig_ValidOutputFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		opts := options.Options{
			UserConfig:     "config.yaml",
			EnabledPlugins: []string{"network-operator"},
			OutputFormat:   format,
		}
		err := validateConfig(&opts)
		assert.NoError(t, err, "format %q should be valid", format)
	}
}

func TestValidateConfig_DryRunRequiresDeploy(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		DryRun:         true,
		Deploy:         false,
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dry-run requires --deploy")
}

func TestValidateConfig_DryRunWithDeployPasses(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		DryRun:         true,
		Deploy:         true,
		Kubeconfig:     "/path/to/kubeconfig",
		Fabric:         "ethernet",
		DeploymentType: "sriov",
		SaveDeploymentFiles: "/tmp/test",
	}
	err := validateConfig(&opts)
	assert.NoError(t, err)
}

func TestValidateConfig_ForRequiresNodeSelector(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		ForPreset:      "PowerEdge-XE9680",
		// NodeSelector intentionally empty
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--for requires --node-selector")
}

func TestValidateConfig_ForRejectsDiscovery(t *testing.T) {
	opts := options.Options{
		UserConfig:            "config.yaml",
		EnabledPlugins:        []string{"network-operator"},
		OutputFormat:          "text",
		ForPreset:             "PowerEdge-XE9680",
		NodeSelector:          "key=val",
		DiscoverClusterConfig: true,
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestValidateConfig_ForWithNodeSelectorPasses(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		ForPreset:      "PowerEdge-XE9680",
		NodeSelector:   "nvidia.com/gpu.product=NVIDIA-H200",
	}
	err := validateConfig(&opts)
	assert.NoError(t, err)
}

func TestResolveKubeconfig(t *testing.T) {
	// Snapshot and restore the package-level defaults so tests don't leak.
	origHome := defaultKubeconfigHomeFile
	origEnv, envSet := os.LookupEnv("KUBECONFIG")
	t.Cleanup(func() {
		defaultKubeconfigHomeFile = origHome
		if envSet {
			_ = os.Setenv("KUBECONFIG", origEnv)
		} else {
			_ = os.Unsetenv("KUBECONFIG")
		}
	})

	t.Run("flag wins over env and home", func(t *testing.T) {
		_ = os.Setenv("KUBECONFIG", "/from/env")
		defaultKubeconfigHomeFile = "/does/not/exist"
		got, err := resolveKubeconfig("/from/flag")
		require.NoError(t, err)
		assert.Equal(t, "/from/flag", got)
	})

	t.Run("env wins over home", func(t *testing.T) {
		_ = os.Setenv("KUBECONFIG", "/from/env")
		defaultKubeconfigHomeFile = "/does/not/exist"
		got, err := resolveKubeconfig("")
		require.NoError(t, err)
		assert.Equal(t, "/from/env", got)
	})

	t.Run("home file used when it exists and nothing else set", func(t *testing.T) {
		_ = os.Unsetenv("KUBECONFIG")
		tmpDir := t.TempDir()
		homeFile := filepath.Join(tmpDir, "config")
		require.NoError(t, os.WriteFile(homeFile, []byte("placeholder"), 0o600))
		defaultKubeconfigHomeFile = homeFile
		got, err := resolveKubeconfig("")
		require.NoError(t, err)
		assert.Equal(t, homeFile, got)
	})

	t.Run("error when home file missing and nothing else set", func(t *testing.T) {
		_ = os.Unsetenv("KUBECONFIG")
		defaultKubeconfigHomeFile = filepath.Join(t.TempDir(), "missing-config")
		got, err := resolveKubeconfig("")
		require.Error(t, err)
		assert.Empty(t, got)
		assert.Contains(t, err.Error(), "no kubeconfig found")
	})
}
