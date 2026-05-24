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
	"strings"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover cluster network hardware capabilities",
	Long: `Bootstrap a private NIC Configuration Daemon into the
nvidia-k8s-launch-kit namespace and use it to discover cluster network
hardware capabilities, producing a cluster-config.yaml file.

Discovery does not require a pre-installed Network Operator: the daemon
and its CRDs are created in a dedicated namespace, used to publish
NicDevice CRs, and torn down when discovery finishes.

Discovery groups nodes by hardware, detects east-west vs north-south
NICs, and probes OFED-dependent modules.`,
	Example: `  # Basic discovery
  l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./cluster-config.yaml

  # Uses $KUBECONFIG if set
  l8k discover --save-cluster-config ./cluster-config.yaml

  # Merge with existing config
  l8k discover --user-config my-config.yaml \
    --save-cluster-config ./cluster-config.yaml

  # Keep the bootstrap namespace for debugging
  l8k discover --kubeconfig ~/.kube/config \
    --keep-namespace \
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
			DiscoverOnly:            true,
			Kubeconfig:              resolved,
			UserConfig:              userConfig,
			SaveClusterConfig:       saveClusterConfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,
			KeepNamespace:            keepNamespace,
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

	discoverCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG, then ~/.kube/config)")
	discoverCmd.Flags().StringVar(&userConfig, "user-config", "", "Base config to merge with discovered hardware")
	discoverCmd.Flags().StringVar(&saveClusterConfig, "save-cluster-config", "", "Output path for cluster-config.yaml")
	discoverCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "(deprecated, no-op for discover) Network Operator namespace override")
	discoverCmd.Flags().StringVar(&networkOperatorRelease, "network-operator-release", "",
		fmt.Sprintf("Network Operator release line to deploy (MAJOR.MINOR). Supported: %s",
			strings.Join(networkoperatorplugin.SupportedReleases(), ", ")))
	discoverCmd.Flags().StringVar(&nodeSelector, "node-selector", "feature.node.kubernetes.io/pci-15b3.present=true", "Filter nodes by label")
	discoverCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for NicClusterPolicy (comma-separated)")
	discoverCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")
	discoverCmd.Flags().BoolVar(&keepNamespace, "keep-namespace", false, "Skip teardown of the nvidia-k8s-launch-kit namespace (for debugging)")

	setFlagGroup(discoverCmd, "kubeconfig", GroupCommon)
	setFlagGroup(discoverCmd, "user-config", GroupCommon)
	setFlagGroup(discoverCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(discoverCmd, "network-operator-release", GroupCommon)
	setFlagGroup(discoverCmd, "node-selector", GroupCommon)
	setFlagGroup(discoverCmd, "image-pull-secrets", GroupCommon)
	setFlagGroup(discoverCmd, "enabled-plugins", GroupCommon)
	setFlagGroup(discoverCmd, "save-cluster-config", GroupDiscovery)
	setFlagGroup(discoverCmd, "keep-namespace", GroupDiscovery)
}
