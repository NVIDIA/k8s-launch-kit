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
	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	hosttarget "github.com/nvidia/k8s-launch-kit/pkg/target/host"
)

var (
	discoverConfigOptions options.Options
	discoverCmd           = &cobra.Command{
		Use:   "discover",
		Short: "Discover cluster network hardware capabilities",
		Long: `Bootstrap a private NIC Configuration Daemon into the
nvidia-k8s-launch-kit namespace and use it to discover cluster network
hardware capabilities, producing a cluster-config.yaml file.

Discovery does not require a pre-installed Network Operator: the daemon
and its CRDs are created in a dedicated namespace, used to publish
NicDevice CRs, and torn down when discovery finishes.

Discovery groups nodes by hardware, detects east-west vs north-south
NICs, and probes OFED-dependent modules. With --user-config, it replaces
only clusterConfig and then applies explicit CLI overrides.`,
		Example: `  # Basic discovery
  l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./cluster-config.yaml

  # Uses $KUBECONFIG if set
  l8k discover --save-cluster-config ./cluster-config.yaml

  # Refresh only clusterConfig in an existing config
  l8k discover --user-config my-config.yaml \
    --save-cluster-config ./cluster-config.yaml

  # Override settings before they are persisted
  l8k discover --kubeconfig ~/.kube/config \
    --fabric infiniband --deployment-type rdma_shared \
    --multirail=false \
    --save-cluster-config ./cluster-config.yaml

  # Keep the bootstrap namespace for debugging
  l8k discover --kubeconfig ~/.kube/config \
    --keep-namespace \
    --save-cluster-config ./cluster-config.yaml

  # Agent mode (JSON output)
  l8k discover --save-cluster-config ./cluster-config.yaml \
    --output json 2>/dev/null`,
		Run: func(cmd *cobra.Command, args []string) {
			opts := mustCollectConfigFlags(cmd, discoverConfigOptions)
			opts.ConfigDir = configDir
			opts.DiscoverClusterConfig = true
			opts.DiscoverOnly = true
			opts.Kubeconfig = kubeconfig
			opts.UserConfig = userConfig
			opts.SaveClusterConfig = saveClusterConfig
			opts.KeepNamespace = keepNamespace
			opts.CollapseNicRails = collapseNicRails
			opts.NodeSelector = nodeSelector
			opts.EnabledPlugins = parseEnabledPlugins(enabledPlugins)
			opts.OutputFormat = outputFormat
			opts.Yes = yesFlag
			opts.Quiet = quietFlag

			runTargetCommand(cmd, target.Discover, hosttarget.NewDiscoverAdapter(
				hosttarget.LauncherRequest{Options: opts},
				hosttarget.NewLauncherRunner(),
			))
		},
	}
)

func init() {
	rootCmd.AddCommand(discoverCmd)
	addTargetFlag(discoverCmd)

	discoverCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG, then ~/.kube/config)")
	discoverCmd.Flags().StringVar(&userConfig, "user-config", "", "Base config to merge with discovered hardware")
	discoverCmd.Flags().StringVar(&saveClusterConfig, "save-cluster-config", "", "Output path for cluster-config.yaml")
	mustBindConfigFlags(discoverCmd, &discoverConfigOptions, configflags.ScopeDiscover)
	discoverCmd.Flags().StringVar(&nodeSelector, "node-selector", "feature.node.kubernetes.io/pci-15b3.present=true", "Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and discoverable NICs are detected via sysfs; restricted BlueFields are excluded")
	discoverCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")
	discoverCmd.Flags().BoolVar(&keepNamespace, "keep-namespace", false, "Skip teardown of the nvidia-k8s-launch-kit namespace (for debugging)")
	discoverCmd.Flags().BoolVar(&collapseNicRails, "collapse-nic-rails", true, collapseNicRailsFlagHelp)

	// Profile settings are resolved after hardware discovery for a fresh config.
	// With --user-config, only explicit flags can change the supplied profile.
	setFlagGroup(discoverCmd, "kubeconfig", GroupCommon)
	setFlagGroup(discoverCmd, "user-config", GroupCommon)
	setFlagGroup(discoverCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(discoverCmd, "network-operator-release", GroupCommon)
	setFlagGroup(discoverCmd, "node-selector", GroupCommon)
	setFlagGroup(discoverCmd, "image-pull-secrets", GroupCommon)
	setFlagGroup(discoverCmd, "enabled-plugins", GroupCommon)
	setFlagGroup(discoverCmd, "save-cluster-config", GroupDiscovery)
	setFlagGroup(discoverCmd, "keep-namespace", GroupDiscovery)
	setFlagGroup(discoverCmd, "collapse-nic-rails", GroupDiscovery)
	setFlagGroup(discoverCmd, "fabric", GroupProfile)
	setFlagGroup(discoverCmd, "deployment-type", GroupProfile)
	setFlagGroup(discoverCmd, "multirail", GroupProfile)
	setFlagGroup(discoverCmd, "routing", GroupProfile)
	setFlagGroup(discoverCmd, "ignore-arp", GroupProfile)
	setFlagGroup(discoverCmd, "spectrum-x", GroupProfile)
	setFlagGroup(discoverCmd, "multiplane-mode", GroupSpectrumX)
	setFlagGroup(discoverCmd, "number-of-planes", GroupSpectrumX)
	setFlagGroup(discoverCmd, "topology-scheme", GroupSpectrumX)
	setFlagGroup(discoverCmd, "ip-version", GroupSpectrumX)
	setFlagGroup(discoverCmd, "topology-file", GroupSpectrumX)
	setFlagGroup(discoverCmd, "spectrum-x-config", GroupSpectrumX)
	setFlagGroup(discoverCmd, "spectrum-x-configmap-name", GroupSpectrumX)
	markDiscoverTargetScopes()
}
