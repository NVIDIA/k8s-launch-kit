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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// generateNodeSelector is the per-subcommand node selector value. It must be
// distinct from the root command's `nodeSelector` package variable: both
// commands bind a `--node-selector` flag, and StringVar sets the default at
// init time — so a single shared var would make the root command's default
// (`feature.node.kubernetes.io/pci-15b3.present=true`) leak into the generate
// flow and break the `--for` requires `--node-selector` validation.
var generateNodeSelector string

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate deployment manifests for a network profile",
	Long: `Select a deployment profile and generate Kubernetes YAML manifests
from a cluster configuration file.

Supports SR-IOV, host-device, RDMA-shared, IPoIB, MacVLAN, and Spectrum-X profiles.
Optionally deploy the generated manifests with --deploy.`,
	Example: `  # SR-IOV Ethernet (most common for GPU clusters)
  l8k generate --user-config cluster-config.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output

  # Spectrum-X with hardware plane load balancing
  l8k generate --user-config cluster-config.yaml \
    --network-operator-release 26.4 \
    --spectrum-x RA2.2 --multiplane-mode hwplb --number-of-planes 4 \
    --save-deployment-files ./output

  # Generate and deploy in one step
  l8k generate --user-config cluster-config.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output \
    --deploy --kubeconfig ~/.kube/config

  # Dry-run: preview what would be deployed
  l8k generate --user-config cluster-config.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output \
    --deploy --dry-run

  # Agent mode (JSON output)
  l8k generate --user-config cluster-config.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output \
    --output json --yes 2>/dev/null

  # Generate from a known server preset (no cluster discovery required)
  l8k generate --user-config cluster-config.yaml \
    --for ThinkSystem-SR680a-V3 \
    --node-selector "feature.node.kubernetes.io/pci-15b3.present=true" \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output`,
	Run: func(cmd *cobra.Command, args []string) {
		// Resolve user config: explicit flag > ./cluster-config.yaml > default config path
		if userConfig == "" {
			if _, err := os.Stat("cluster-config.yaml"); err == nil {
				userConfig = "cluster-config.yaml"
			} else if resolved := app.DefaultConfigPath(); resolved != "" {
				userConfig = resolved
			} else {
				exitWithError(apperrors.NewValidationError(
					"no configuration file found",
					fmt.Errorf("checked ./cluster-config.yaml, ./l8k-config.yaml, and installed paths"),
					"Run 'l8k discover' first, or pass --user-config <path>",
				), outputFormat)
			}
		}

		opts := options.Options{
			UserConfig:              userConfig,
			Fabric:                  fabric,
			DeploymentType:          deploymentType,
			Multirail:               multirail,
			SpectrumX:               spectrumXVersion != "",
			SPCXVersion:             spectrumXVersion,
			MultiplaneMode:          multiplaneMode,
			NumberOfPlanes:          numberOfPlanes,
			Groups:                  groups,
			GpuType:                 gpuType,
			NodeSelector:            generateNodeSelector,
			ForPreset:               forPreset,
			ImagePullSecrets:        imagePullSecrets,
			PodNamespace:            podNamespace,
			SaveDeploymentFiles:     saveDeploymentFiles,
			Deploy:                  deploy,
			Kubeconfig:              kubeconfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,
			EnabledPlugins:           parseEnabledPlugins(enabledPlugins),
			WorkloadManifest:        workloadManifest,
			OutputFormat:             outputFormat,
			Yes:                      yesFlag,
			Quiet:                    quietFlag,
			DryRun:                   dryRunFlag,
		}

		// Set EnableDocaDriver only if the flag was explicitly provided
		if cmd.Flags().Lookup("enable-doca-driver").Changed {
			opts.EnableDocaDriver = &enableDocaDriver
		}

		if err := applySpectrumXDefaults(&opts); err != nil {
			exitWithError(apperrors.NewValidationError(err.Error(), err, "Check --spectrum-x flag combinations"), opts.OutputFormat)
		}

		// --for requires --node-selector since the preset has no live worker-node
		// list to target.
		if opts.ForPreset != "" && opts.NodeSelector == "" {
			exitWithError(apperrors.NewValidationError(
				"--for requires --node-selector",
				fmt.Errorf("preset %q has no live worker-node list; supply a node selector to identify target nodes", opts.ForPreset),
				"Specify --node-selector key1=val1,key2=val2",
			), opts.OutputFormat)
		}

		// Resolve kubeconfig if deploy is requested
		if opts.Deploy {
			resolved, err := resolveKubeconfig(opts.Kubeconfig)
			if err != nil {
				exitWithError(apperrors.NewValidationError(
					"kubeconfig required for deployment",
					err,
					"Set $KUBECONFIG or pass --kubeconfig <path>",
				), opts.OutputFormat)
			}
			opts.Kubeconfig = resolved
		}

		launcher := app.New(opts)
		if err := launcher.Run(); err != nil {
			var se *apperrors.StructuredError
			if !errors.As(err, &se) {
				se = apperrors.NewGeneralError(err.Error(), err)
			}
			exitWithError(se, opts.OutputFormat)
		}
	},
}

func init() {
	rootCmd.AddCommand(generateCmd)

	// Config
	generateCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file (auto-detected from ./cluster-config.yaml or installed path)")

	// Profile selection
	generateCmd.Flags().StringVar(&fabric, "fabric", "", "Fabric type: ethernet, infiniband")
	generateCmd.Flags().StringVar(&deploymentType, "deployment-type", "", "Deployment type: sriov, rdma_shared, host_device")
	generateCmd.Flags().BoolVar(&multirail, "multirail", false, "Enable multirail deployment")
	generateCmd.Flags().StringVar(&spectrumXVersion, "spectrum-x", "",
		fmt.Sprintf("Enable Spectrum-X by passing the SPC-X RA version (e.g. RA2.1, RA2.2). Supported: %v",
			config.SupportedSPCXVersions))
	generateCmd.Flags().StringVar(&multiplaneMode, "multiplane-mode", "", "Multiplane mode: swplb, hwplb, uniplane (requires --spectrum-x)")
	generateCmd.Flags().IntVar(&numberOfPlanes, "number-of-planes", 0, "Number of planes (requires --spectrum-x)")
	generateCmd.Flags().StringSliceVar(&groups, "groups", nil, "Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.")
	generateCmd.Flags().StringVar(&gpuType, "gpu-type", "", "Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.")
	generateCmd.MarkFlagsMutuallyExclusive("groups", "gpu-type")
	generateCmd.Flags().StringVar(&forPreset, "for", "", forFlagHelp())
	generateCmd.Flags().StringVar(&generateNodeSelector, "node-selector", "", "Node selector for the synthesized clusterConfig when --for is used (e.g., key=value,key2=value2). Required with --for.")

	// Output
	generateCmd.Flags().StringVar(&saveDeploymentFiles, "save-deployment-files", "", "Output directory for generated YAMLs")
	generateCmd.Flags().StringVar(&podNamespace, "pod-namespace", "", "Namespace for pods and network resources")
	generateCmd.Flags().BoolVar(&enableDocaDriver, "enable-doca-driver", false, "Enable DOCA driver deployment")
	generateCmd.Flags().StringVar(&workloadManifest, "workload-manifest", "", "Path to a custom workload manifest YAML")
	generateCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override operator namespace")
	generateCmd.Flags().StringVar(&networkOperatorRelease, "network-operator-release", "",
		fmt.Sprintf("Network Operator release line to deploy (MAJOR.MINOR). Supported: %s",
			strings.Join(networkoperatorplugin.SupportedReleases(), ", ")))
	generateCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for NicClusterPolicy (comma-separated)")
	generateCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")

	// Deploy (optional)
	generateCmd.Flags().BoolVar(&deploy, "deploy", false, "Also deploy after generating")
	generateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig (required with --deploy, falls back to $KUBECONFIG)")
	generateCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Preview what would be deployed")

	setFlagGroup(generateCmd, "user-config", GroupCommon)
	setFlagGroup(generateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(generateCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(generateCmd, "network-operator-release", GroupCommon)
	setFlagGroup(generateCmd, "image-pull-secrets", GroupCommon)
	setFlagGroup(generateCmd, "enabled-plugins", GroupCommon)

	setFlagGroup(generateCmd, "fabric", GroupProfile)
	setFlagGroup(generateCmd, "deployment-type", GroupProfile)
	setFlagGroup(generateCmd, "multirail", GroupProfile)
	setFlagGroup(generateCmd, "spectrum-x", GroupProfile)
	setFlagGroup(generateCmd, "groups", GroupProfile)
	setFlagGroup(generateCmd, "gpu-type", GroupProfile)
	setFlagGroup(generateCmd, "for", GroupProfile)
	setFlagGroup(generateCmd, "node-selector", GroupProfile)

	setFlagGroup(generateCmd, "multiplane-mode", GroupSpectrumX)
	setFlagGroup(generateCmd, "number-of-planes", GroupSpectrumX)

	setFlagGroup(generateCmd, "save-deployment-files", GroupGeneration)
	setFlagGroup(generateCmd, "pod-namespace", GroupGeneration)
	setFlagGroup(generateCmd, "enable-doca-driver", GroupGeneration)
	setFlagGroup(generateCmd, "workload-manifest", GroupGeneration)

	setFlagGroup(generateCmd, "deploy", GroupDeploy)
	setFlagGroup(generateCmd, "dry-run", GroupDeploy)
}
