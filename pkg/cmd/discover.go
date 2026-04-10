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

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover cluster network hardware capabilities",
	Long: `Deploy a minimal Network Operator profile to discover cluster network
hardware capabilities and produce a cluster-config.yaml file.

Discovery inspects NodeFeature CRDs, groups nodes by hardware, detects
east-west vs north-south NICs, and probes OFED dependent modules.`,
	Example: `  # Basic discovery
  l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./cluster-config.yaml

  # Uses $KUBECONFIG if set
  l8k discover --save-cluster-config ./cluster-config.yaml

  # Non-default operator namespace
  l8k discover --kubeconfig ~/.kube/config \
    --network-operator-namespace network-operator \
    --save-cluster-config ./cluster-config.yaml

  # Merge with existing config
  l8k discover --user-config my-config.yaml \
    --save-cluster-config ./cluster-config.yaml

  # Agent mode (JSON output)
  l8k discover --save-cluster-config ./cluster-config.yaml \
    --output json --yes 2>/dev/null`,
	Run: func(cmd *cobra.Command, args []string) {
		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"kubeconfig required for discovery",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			), outputFormat)
		}

		opts := options.Options{
			DiscoverClusterConfig:   true,
			Kubeconfig:              resolved,
			UserConfig:              userConfig,
			SaveClusterConfig:       saveClusterConfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NodeSelector:            nodeSelector,
			ImagePullSecrets:        imagePullSecrets,
			EnabledPlugins:           parseEnabledPlugins(enabledPlugins),
			OutputFormat:             outputFormat,
			Yes:                      yesFlag,
			Quiet:                    quietFlag,
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
	rootCmd.AddCommand(discoverCmd)

	discoverCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG)")
	discoverCmd.Flags().StringVar(&userConfig, "user-config", "", "Base config to merge with discovered hardware")
	discoverCmd.Flags().StringVar(&saveClusterConfig, "save-cluster-config", "", "Output path for cluster-config.yaml")
	discoverCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override operator namespace (default: nvidia-network-operator)")
	discoverCmd.Flags().StringVar(&nodeSelector, "node-selector", "feature.node.kubernetes.io/pci-15b3.present=true", "Filter nodes by label")
	discoverCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for NicClusterPolicy (comma-separated)")
	discoverCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")
}
