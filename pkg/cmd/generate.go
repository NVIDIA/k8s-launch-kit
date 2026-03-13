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

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

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
    --spectrum-x --spcx-version RA2.1 --multiplane-mode hwplb \
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
    --output json --yes 2>/dev/null`,
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
			SpectrumX:               spectrumX,
			Ai:                      ai,
			SPCXVersion:             spcxVersion,
			MultiplaneMode:          multiplaneMode,
			NumberOfPlanes:          numberOfPlanes,
			Group:                   group,
			LabelSelector:           labelSelector,
			PodNamespace:            podNamespace,
			SaveDeploymentFiles:     saveDeploymentFiles,
			Deploy:                  deploy,
			Kubeconfig:              kubeconfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			EnabledPlugins:           parseEnabledPlugins(enabledPlugins),
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
	generateCmd.Flags().BoolVar(&spectrumX, "spectrum-x", false, "Enable Spectrum-X deployment")
	generateCmd.Flags().BoolVar(&ai, "ai", false, "Enable AI deployment")
	generateCmd.Flags().StringVar(&spcxVersion, "spcx-version", "", "Spectrum-X version (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&multiplaneMode, "multiplane-mode", "", "Multiplane mode: swplb, hwplb, uniplane (requires --spectrum-x)")
	generateCmd.Flags().IntVar(&numberOfPlanes, "number-of-planes", 0, "Number of planes (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&group, "group", "", "Generate for a specific group only (e.g., group-0)")

	// Output
	generateCmd.Flags().StringVar(&saveDeploymentFiles, "save-deployment-files", "", "Output directory for generated YAMLs")
	generateCmd.Flags().StringVar(&podNamespace, "pod-namespace", "", "Namespace for pods and network resources")
	generateCmd.Flags().BoolVar(&enableDocaDriver, "enable-doca-driver", false, "Enable DOCA driver deployment")
	generateCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override operator namespace")
	generateCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")

	// Deploy (optional)
	generateCmd.Flags().BoolVar(&deploy, "deploy", false, "Also deploy after generating")
	generateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig (required with --deploy, falls back to $KUBECONFIG)")
	generateCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Preview what would be deployed")
}
