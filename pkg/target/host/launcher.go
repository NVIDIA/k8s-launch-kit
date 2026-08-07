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

package host

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

type launcherRunner struct{}

// NewLauncherRunner returns the production app.Launcher-backed Host runner.
func NewLauncherRunner() LauncherRunner {
	return launcherRunner{}
}

func (launcherRunner) Run(ctx context.Context, phase target.Phase, opts options.Options) error {
	prepared := cloneOptions(opts)
	if err := prepareLauncherOptions(phase, &prepared); err != nil {
		return err
	}
	return app.New(prepared).RunContext(ctx)
}

func prepareLauncherOptions(phase target.Phase, opts *options.Options) error {
	switch phase {
	case target.Discover:
		resolved, err := ResolveKubeconfig(opts.Kubeconfig)
		if err != nil {
			return apperrors.NewValidationError(
				"kubeconfig required for discovery",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			)
		}
		opts.Kubeconfig = resolved
	case target.Generate:
		if opts.UserConfig == "" {
			resolved := UserConfigPathForGenerate(UserConfigInput{ConfigDir: opts.ConfigDir})
			if resolved != "" {
				opts.UserConfig = resolved
			} else if opts.ForPreset == "" && opts.ConfigDir == "" {
				return apperrors.NewValidationError(
					"no configuration file found",
					fmt.Errorf("checked ./cluster-config.yaml, --config-dir, ./l8k-config.yaml, and installed paths"),
					"Run 'l8k discover' first, pass --user-config <path>, or use --for with the embedded default",
				)
			}
		}
		if opts.Deploy {
			resolved, err := ResolveKubeconfig(opts.Kubeconfig)
			if err != nil {
				return apperrors.NewValidationError(
					"kubeconfig required for deployment",
					err,
					"Set $KUBECONFIG or pass --kubeconfig <path>",
				)
			}
			opts.Kubeconfig = resolved
		}
	case target.Pipeline:
		if opts.DiscoverClusterConfig || opts.Deploy {
			resolved, err := ResolveKubeconfig(opts.Kubeconfig)
			if err != nil {
				return apperrors.NewValidationError(
					"kubeconfig required for cluster operations: set $KUBECONFIG or pass --kubeconfig <path>",
					err,
					"Run 'l8k --help' for usage information",
				)
			}
			opts.Kubeconfig = resolved
		}
	default:
		return fmt.Errorf("host launcher does not support phase %q", phase)
	}
	return nil
}

func validateLauncherOptions(phase target.Phase, opts *options.Options) error {
	if opts == nil {
		return fmt.Errorf("host launcher options must not be nil")
	}
	if err := ValidateProfileFlagValues(opts); err != nil {
		suggestion := "Check --spectrum-x flag combinations"
		if phase == target.Discover {
			suggestion = "Check profile selection flags"
		}
		return apperrors.NewValidationError(err.Error(), err, suggestion)
	}
	switch phase {
	case target.Discover:
		return nil
	case target.Generate:
		if opts.ForPreset != "" && opts.NodeSelector == "" {
			cause := fmt.Errorf("preset %q has no live worker-node list; supply a node selector to identify target nodes", opts.ForPreset)
			return apperrors.NewValidationError("--for requires --node-selector", cause,
				"Specify --node-selector key1=val1,key2=val2")
		}
		return nil
	case target.Pipeline:
		if err := ValidatePipelineOptions(opts); err != nil {
			return apperrors.NewValidationError(err.Error(), err, "Run 'l8k --help' for usage information")
		}
		return nil
	default:
		return fmt.Errorf("host launcher does not support phase %q", phase)
	}
}

// ValidatePipelineOptions validates Host root-pipeline flag combinations. Path
// fallback and kubeconfig resolution remain execution-time Host concerns.
func ValidatePipelineOptions(opts *options.Options) error {
	if opts == nil {
		return fmt.Errorf("options must not be nil")
	}
	if !slices.Contains([]string{"text", "json"}, opts.OutputFormat) {
		return fmt.Errorf("--output must be one of: text, json")
	}
	if opts.NetworkOperatorRelease != "" {
		if _, ok := releases.LookupRelease(opts.NetworkOperatorRelease); !ok {
			return fmt.Errorf("unknown --network-operator-release %q; supported: %v",
				opts.NetworkOperatorRelease, releases.SupportedReleases())
		}
	}
	if opts.DryRun && !opts.Deploy {
		return fmt.Errorf("--dry-run requires --deploy to be specified (it previews what deploy would do)")
	}
	if opts.WorkloadManifest != "" {
		if _, err := os.Stat(opts.WorkloadManifest); os.IsNotExist(err) {
			return fmt.Errorf("workload manifest file does not exist: %s", opts.WorkloadManifest)
		}
	}
	if len(opts.EnabledPlugins) == 0 {
		return fmt.Errorf("no plugins enabled, use --enabled-plugins to enable plugins")
	}
	if opts.UserConfig == "" && opts.ConfigDir == "" && opts.ForPreset == "" && !opts.DiscoverClusterConfig {
		return fmt.Errorf("one of --user-config, --config-dir, --for, or --discover-cluster-config must be provided")
	}
	if opts.ForPreset != "" {
		if opts.DiscoverClusterConfig {
			return fmt.Errorf("--for and --discover-cluster-config are mutually exclusive")
		}
		if opts.NodeSelector == "" {
			return fmt.Errorf("--for requires --node-selector (specify which nodes the synthesized clusterConfig should target)")
		}
	}
	if slices.Contains(opts.EnabledPlugins, networkoperatorplugin.PluginName) {
		if (opts.Fabric != "" || opts.DeploymentType != "") && opts.SaveDeploymentFiles == "" && !opts.Deploy {
			return fmt.Errorf("when --deployment-type is specified, either --save-deployment-files or --deploy must be provided")
		}
		if opts.Fabric == "" && opts.DeploymentType == "" && opts.UserConfig == "" && opts.Deploy {
			return fmt.Errorf("--deploy requires --deployment-type or --user-config with a profile to be specified")
		}
		if (opts.DeploymentType != "" && opts.Fabric == "") || (opts.Fabric != "" && opts.DeploymentType == "") {
			return fmt.Errorf("--deployment-type requires --fabric to be specified")
		}
		if opts.Fabric != "" && !slices.Contains([]string{"infiniband", "ethernet"}, opts.Fabric) {
			return fmt.Errorf("--fabric must be one of: infiniband, ethernet")
		}
		if opts.DeploymentType != "" && !slices.Contains([]string{"sriov", "rdma_shared", "host_device"}, opts.DeploymentType) {
			return fmt.Errorf("--deployment-type must be one of: sriov, rdma_shared, host_device")
		}
	}
	return nil
}

// ValidateProfileFlagValues validates Host profile enums before cluster-side
// work. Partial profiles remain valid because resolution fills missing values.
func ValidateProfileFlagValues(opts *options.Options) error {
	if opts == nil {
		return fmt.Errorf("options must not be nil")
	}
	if opts.Fabric != "" && !slices.Contains([]string{"infiniband", "ethernet"}, opts.Fabric) {
		return fmt.Errorf("--fabric must be one of: infiniband, ethernet")
	}
	if opts.DeploymentType != "" && !slices.Contains([]string{"sriov", "rdma_shared", "host_device"}, opts.DeploymentType) {
		return fmt.Errorf("--deployment-type must be one of: sriov, rdma_shared, host_device")
	}
	if opts.Routing != "" && !slices.Contains([]string{config.RoutingDestinationBased, config.RoutingSourceBased}, opts.Routing) {
		return fmt.Errorf("--routing must be one of: %s, %s", config.RoutingDestinationBased, config.RoutingSourceBased)
	}
	return ValidateSpectrumXSyntax(opts)
}

// ValidateSpectrumXSyntax validates Host Spectrum-X CLI syntax before
// hardware defaults and resolved-config validation run.
func ValidateSpectrumXSyntax(opts *options.Options) error {
	if opts == nil {
		return fmt.Errorf("options must not be nil")
	}
	if !opts.SpectrumX {
		switch {
		case opts.MultiplaneMode != "":
			return fmt.Errorf("--multiplane-mode can only be used with --spectrum-x")
		case opts.NumberOfPlanes != 0:
			return fmt.Errorf("--number-of-planes can only be used with --spectrum-x")
		case opts.TopologyScheme != "":
			return fmt.Errorf("--topology-scheme can only be used with --spectrum-x")
		case opts.IPVersion != "":
			return fmt.Errorf("--ip-version can only be used with --spectrum-x")
		case opts.TopologyFile != "":
			return fmt.Errorf("--topology-file can only be used with --spectrum-x")
		case opts.SpectrumXConfig != "":
			return fmt.Errorf("--spectrum-x-config can only be used with --spectrum-x")
		case opts.SpectrumXConfigMapName != "":
			return fmt.Errorf("--spectrum-x-configmap-name can only be used with --spectrum-x")
		default:
			return nil
		}
	}
	if opts.SPCXVersion == "" {
		return fmt.Errorf("--spectrum-x requires the SPC-X RA version as its value; supported: %v", config.SupportedSPCXVersions)
	}
	if !slices.Contains(config.SupportedSPCXVersions, opts.SPCXVersion) {
		return fmt.Errorf("invalid --spectrum-x value %q; supported: %v", opts.SPCXVersion, config.SupportedSPCXVersions)
	}
	if opts.MultiplaneMode != "" && !slices.Contains(config.SupportedMultiplaneModes, opts.MultiplaneMode) {
		return fmt.Errorf("invalid --multiplane-mode %q; supported: %v", opts.MultiplaneMode, config.SupportedMultiplaneModes)
	}
	if opts.NumberOfPlanes != 0 && !slices.Contains(config.SupportedNumberOfPlanes, opts.NumberOfPlanes) {
		return fmt.Errorf("invalid --number-of-planes %d; supported: %v", opts.NumberOfPlanes, config.SupportedNumberOfPlanes)
	}
	if opts.TopologyScheme != "" && !slices.Contains(config.SupportedSpectrumXTopologyTypes, opts.TopologyScheme) {
		return fmt.Errorf("invalid --topology-scheme %q; supported: %v", opts.TopologyScheme, config.SupportedSpectrumXTopologyTypes)
	}
	if opts.IPVersion != "" && !slices.Contains(config.SupportedSpectrumXIPVersions, opts.IPVersion) {
		return fmt.Errorf("invalid --ip-version %q; supported: %v", opts.IPVersion, config.SupportedSpectrumXIPVersions)
	}
	if opts.NetworkOperatorRelease != "" {
		allowed := config.SPCXVersionAllowedReleases[opts.SPCXVersion]
		if !slices.Contains(allowed, opts.NetworkOperatorRelease) {
			return fmt.Errorf("--spectrum-x %s requires --network-operator-release in %v, got %s",
				opts.SPCXVersion, allowed, opts.NetworkOperatorRelease)
		}
	}
	return nil
}
