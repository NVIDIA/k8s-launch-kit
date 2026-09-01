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
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	hosttarget "github.com/nvidia/k8s-launch-kit/pkg/target/host"
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
File-backed configs are rewritten with resolved defaults and CLI overrides.
Optionally deploy the generated manifests with --deploy.`,
	Example: `  # SR-IOV Ethernet (most common for GPU clusters)
  l8k generate --user-config cluster-config.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output

  # Spectrum-X with hardware plane load balancing
  l8k generate --user-config cluster-config.yaml \
    --network-operator-release 26.7 \
    --spectrum-x RA2.3 --multiplane-mode hwplb --number-of-planes 4 \
    --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
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

  # Generate from a known server preset using the embedded default config
  l8k generate \
    --for ThinkSystem-SR680a-V3-H200 \
    --node-selector "feature.node.kubernetes.io/pci-15b3.present=true" \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./output`,
	Run: func(cmd *cobra.Command, args []string) {
		opts := options.Options{
			LaunchKitVersion:         Version,
			ConfigDir:                configDir,
			UserConfig:               userConfig,
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
			Groups:                   groups,
			GpuType:                  gpuType,
			NodeSelector:             generateNodeSelector,
			ForPreset:                forPreset,
			ImagePullSecrets:         imagePullSecrets,
			NetworkNamespaces:        networkNamespaces,
			SaveDeploymentFiles:      saveDeploymentFiles,
			Deploy:                   deploy,
			OverwriteExisting:        overwriteExistingFlag,
			Kubeconfig:               kubeconfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,

			SkipNetworkOperatorHelm:    skipNetworkOperatorHelm,
			SkipNetworkOperatorHelmSet: cmd.Flag("skip-network-operator-helm").Changed,

			EnabledPlugins:   parseEnabledPlugins(enabledPlugins),
			WorkloadManifest: workloadManifest,
			OutputFormat:     outputFormat,
			Yes:              yesFlag,
			Quiet:            quietFlag,
			DryRun:           dryRunFlag,
		}

		// Set EnableDocaDriver only if the flag was explicitly provided
		if cmd.Flags().Lookup("enable-doca-driver").Changed {
			opts.EnableDocaDriver = &enableDocaDriver
		}

		runTargetCommand(cmd, target.Generate, hosttarget.NewGenerateAdapter(
			hosttarget.LauncherRequest{Options: opts},
			hosttarget.NewLauncherRunner(),
		))
	},
}

func init() {
	rootCmd.AddCommand(generateCmd)
	addTargetFlag(generateCmd)

	// Config
	generateCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file (otherwise auto-detected from ./cluster-config.yaml; --for can use embedded defaults)")

	// Profile selection
	generateCmd.Flags().StringVar(&fabric, "fabric", "", "Fabric type: ethernet, infiniband")
	generateCmd.Flags().StringVar(&deploymentType, "deployment-type", "", "Deployment type: sriov, rdma_shared, host_device")
	generateCmd.Flags().BoolVar(&multirail, "multirail", false, "Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)")
	generateCmd.Flags().StringVar(&routing, "routing", "", "Secondary-network routing mode: destination-based or source-based. source-based chains the automatic sbr CNI meta-plugin.")
	generateCmd.Flags().BoolVar(&ignoreARP, "ignore-arp", false, "Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails")
	generateCmd.Flags().StringVar(&spectrumXVersion, "spectrum-x", "",
		fmt.Sprintf("Enable Spectrum-X by passing the SPC-X RA version (e.g. RA2.1, RA2.2). Supported: %v",
			config.SupportedSPCXVersions))
	generateCmd.Flags().StringVar(&multiplaneMode, "multiplane-mode", "", "Multiplane mode: none, swplb, hwplb (requires --spectrum-x)")
	generateCmd.Flags().IntVar(&numberOfPlanes, "number-of-planes", 0, "Number of planes (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&topologyScheme, "topology-scheme", "", "Spectrum-X topology scheme for guide-based IP allocation: 2-tier or 3-tier (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&ipVersion, "ip-version", "", "Spectrum-X IP version for guide-based allocation: ipv4 or ipv6 (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&topologyFile, "topology-file", "", "Path to spcx-gen/reference-generator or NVIDIA AIR topology JSON for Spectrum-X CIDRPool generation (requires --spectrum-x)")
	generateCmd.Flags().StringVar(&spectrumXConfig, "spectrum-x-config", "", "Path to full Spectrum-X profile ConfigMap YAML or raw data.profile YAML (required for SPC-X RA versions newer than RA2.2)")
	generateCmd.Flags().StringVar(&spectrumXConfigMapName, "spectrum-x-configmap-name", "", "Spectrum-X profile ConfigMap name when --spectrum-x-config contains raw data.profile YAML")
	generateCmd.Flags().StringSliceVar(&groups, "groups", nil, "Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.")
	generateCmd.Flags().StringVar(&gpuType, "gpu-type", "", "Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.")
	generateCmd.MarkFlagsMutuallyExclusive("groups", "gpu-type")
	generateCmd.Flags().StringVar(&forPreset, "for", "", forFlagHelp())
	generateCmd.Flags().StringVar(&generateNodeSelector, "node-selector", "", "Node selector for the synthesized clusterConfig when --for is used (e.g., key=value,key2=value2). Required with --for.")

	// Output
	generateCmd.Flags().StringVar(&saveDeploymentFiles, "save-deployment-files", "", "Output directory for generated YAMLs")
	generateCmd.Flags().StringSliceVar(&networkNamespaces, "network-namespaces", nil, "Comma-separated namespaces for the secondary-network CRs and example test DaemonSets. One independent copy is rendered per namespace (shared resources like IPPools and NodePolicies are NOT duplicated). Overrides config networkNamespaces; default: 'default'.")
	generateCmd.Flags().BoolVar(&enableDocaDriver, "enable-doca-driver", false, "Enable DOCA driver deployment")
	generateCmd.Flags().StringVar(&workloadManifest, "workload-manifest", "", "Path to a custom workload manifest YAML")
	generateCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override operator namespace")
	generateCmd.Flags().StringVar(&networkOperatorRelease, "network-operator-release", "",
		fmt.Sprintf("Network Operator release line to deploy (MAJOR.MINOR). Supported: %s",
			strings.Join(releases.SupportedReleases(), ", ")))
	generateCmd.Flags().BoolVar(&skipNetworkOperatorHelm, "skip-network-operator-helm", false, "Skip Network Operator Helm values generation and, with --deploy, chart installation and Helm preflight checks")
	generateCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for Network Operator components and authenticated Helm downloads (comma-separated)")
	generateCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")

	// Deploy (optional)
	generateCmd.Flags().BoolVar(&deploy, "deploy", false, "Also deploy after generating")
	generateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig (required with --deploy, falls back to $KUBECONFIG, then ~/.kube/config)")
	generateCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Preview what would be deployed")
	generateCmd.Flags().BoolVar(&overwriteExistingFlag, "overwrite-existing", false, "Converge the cluster to the rendered manifests when preflight detects drift: helm upgrade the chart on chart-version/values mismatch, delete stray Network Operator CRs in the operator namespace, and rewrite NicClusterPolicy component versions via SSA. Off by default — preflight fails fast and lists what would change.")

	setFlagGroup(generateCmd, "user-config", GroupCommon)
	setFlagGroup(generateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(generateCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(generateCmd, "network-operator-release", GroupCommon)
	setFlagGroup(generateCmd, "skip-network-operator-helm", GroupCommon)
	setFlagGroup(generateCmd, "image-pull-secrets", GroupCommon)
	setFlagGroup(generateCmd, "enabled-plugins", GroupCommon)

	setFlagGroup(generateCmd, "fabric", GroupProfile)
	setFlagGroup(generateCmd, "deployment-type", GroupProfile)
	setFlagGroup(generateCmd, "multirail", GroupProfile)
	setFlagGroup(generateCmd, "routing", GroupProfile)
	setFlagGroup(generateCmd, "ignore-arp", GroupProfile)
	setFlagGroup(generateCmd, "spectrum-x", GroupProfile)
	setFlagGroup(generateCmd, "groups", GroupProfile)
	setFlagGroup(generateCmd, "gpu-type", GroupProfile)
	setFlagGroup(generateCmd, "for", GroupProfile)
	setFlagGroup(generateCmd, "node-selector", GroupProfile)

	setFlagGroup(generateCmd, "multiplane-mode", GroupSpectrumX)
	setFlagGroup(generateCmd, "number-of-planes", GroupSpectrumX)
	setFlagGroup(generateCmd, "topology-scheme", GroupSpectrumX)
	setFlagGroup(generateCmd, "ip-version", GroupSpectrumX)
	setFlagGroup(generateCmd, "topology-file", GroupSpectrumX)
	setFlagGroup(generateCmd, "spectrum-x-config", GroupSpectrumX)
	setFlagGroup(generateCmd, "spectrum-x-configmap-name", GroupSpectrumX)

	setFlagGroup(generateCmd, "save-deployment-files", GroupGeneration)
	setFlagGroup(generateCmd, "network-namespaces", GroupGeneration)
	setFlagGroup(generateCmd, "enable-doca-driver", GroupGeneration)
	setFlagGroup(generateCmd, "workload-manifest", GroupGeneration)

	setFlagGroup(generateCmd, "deploy", GroupExecution)
	setFlagGroup(generateCmd, "dry-run", GroupExecution)
	setFlagGroup(generateCmd, "overwrite-existing", GroupDeploy)
	markGenerateTargetScopes()
}
