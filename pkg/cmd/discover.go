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
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
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
NICs, probes OFED-dependent modules, resolves missing profile settings,
and writes the final profile back to cluster-config.yaml.`,
	Example: `  # Basic discovery
  l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./cluster-config.yaml

  # Uses $KUBECONFIG if set
  l8k discover --save-cluster-config ./cluster-config.yaml

  # Merge with existing config
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
		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"kubeconfig required for discovery",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			), outputFormat)
		}

		opts := options.Options{
			ConfigDir:                configDir,
			DiscoverClusterConfig:    true,
			DiscoverOnly:             true,
			Kubeconfig:               resolved,
			UserConfig:               userConfig,
			SaveClusterConfig:        saveClusterConfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,
			Fabric:                   fabric,
			DeploymentType:           deploymentType,
			Multirail:                multirail,
			MultirailSet:             cmd.Flag("multirail").Changed,
			Routing:                  routing,
			IgnoreARP:                ignoreARP,
			IgnoreARPSet:             cmd.Flag("ignore-arp").Changed,
			SpectrumX:                spectrumXVersion != "",
			SPCXVersion:              spectrumXVersion,
			MultiplaneMode:           multiplaneMode,
			NumberOfPlanes:           numberOfPlanes,
			TopologyScheme:           topologyScheme,
			IPVersion:                ipVersion,
			TopologyFile:             topologyFile,
			SpectrumXConfig:          spectrumXConfig,
			SpectrumXConfigMapName:   spectrumXConfigMapName,
			KeepNamespace:            keepNamespace,
			CollapseNicRails:         collapseNicRails,
			NodeSelector:             nodeSelector,
			ImagePullSecrets:         imagePullSecrets,
			EnabledPlugins:           parseEnabledPlugins(enabledPlugins),
			OutputFormat:             outputFormat,
			Yes:                      yesFlag,
			Quiet:                    quietFlag,
		}

		if err := validateProfileFlagValues(&opts); err != nil {
			exitWithError(apperrors.NewValidationError(
				err.Error(), err, "Check profile selection flags"), opts.OutputFormat)
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
			strings.Join(releases.SupportedReleases(), ", ")))
	discoverCmd.Flags().StringVar(&nodeSelector, "node-selector", "feature.node.kubernetes.io/pci-15b3.present=true", "Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe")
	discoverCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for NicClusterPolicy (comma-separated)")
	discoverCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")
	discoverCmd.Flags().BoolVar(&keepNamespace, "keep-namespace", false, "Skip teardown of the nvidia-k8s-launch-kit namespace (for debugging)")
	discoverCmd.Flags().BoolVar(&collapseNicRails, "collapse-nic-rails", true, collapseNicRailsFlagHelp)

	// Profile settings are resolved after hardware discovery and persisted in
	// cluster-config.yaml. Explicit flags override values from --user-config.
	discoverCmd.Flags().StringVar(&fabric, "fabric", "", "Fabric type override: ethernet, infiniband")
	discoverCmd.Flags().StringVar(&deploymentType, "deployment-type", "", "Deployment type override: sriov, rdma_shared, host_device")
	discoverCmd.Flags().BoolVar(&multirail, "multirail", false, "Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)")
	discoverCmd.Flags().StringVar(&routing, "routing", "", "Secondary-network routing mode override: destination-based or source-based. source-based chains the automatic sbr CNI meta-plugin.")
	discoverCmd.Flags().BoolVar(&ignoreARP, "ignore-arp", false, "Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails")
	discoverCmd.Flags().StringVar(&spectrumXVersion, "spectrum-x", "",
		fmt.Sprintf("Enable Spectrum-X by passing the SPC-X RA version. Supported: %v",
			config.SupportedSPCXVersions))
	discoverCmd.Flags().StringVar(&multiplaneMode, "multiplane-mode", "", "Spectrum-X multiplane mode override: none, swplb, hwplb (requires --spectrum-x)")
	discoverCmd.Flags().IntVar(&numberOfPlanes, "number-of-planes", 0, "Spectrum-X plane count override: 1, 2, or 4 (requires --spectrum-x)")
	discoverCmd.Flags().StringVar(&topologyScheme, "topology-scheme", "", "Spectrum-X topology scheme for guide-based IP allocation: 2-tier or 3-tier (requires --spectrum-x)")
	discoverCmd.Flags().StringVar(&ipVersion, "ip-version", "", "Spectrum-X IP version for guide-based allocation: ipv4 or ipv6 (requires --spectrum-x)")
	discoverCmd.Flags().StringVar(&topologyFile, "topology-file", "", "Path to spcx-gen/reference-generator or NVIDIA AIR topology JSON for Spectrum-X CIDRPool generation (requires --spectrum-x)")
	discoverCmd.Flags().StringVar(&spectrumXConfig, "spectrum-x-config", "", "Path to full Spectrum-X profile ConfigMap YAML or raw data.profile YAML (required for SPC-X RA versions newer than RA2.2)")
	discoverCmd.Flags().StringVar(&spectrumXConfigMapName, "spectrum-x-configmap-name", "", "Spectrum-X profile ConfigMap name when --spectrum-x-config contains raw data.profile YAML")

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
}
