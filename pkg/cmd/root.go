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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	applog "github.com/nvidia/k8s-launch-kit/pkg/log"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

var (
	logLevel       string
	logFile        string
	configDir      string
	fabric         string
	deploymentType string
	multirail      bool
	routing        string
	ignoreARP      bool
	// spectrumXVersion holds the value of --spectrum-x. Empty means
	// Spectrum-X is disabled; a non-empty value is the SPC-X RA version
	// (validated against config.SupportedSPCXVersions). The legacy --spcx-version
	// flag has been folded into --spectrum-x; passing the version is now part
	// of opting into Spectrum-X.
	spectrumXVersion         string
	multiplaneMode           string
	numberOfPlanes           int
	topologyScheme           string
	ipVersion                string
	topologyFile             string
	spectrumXConfig          string
	spectrumXConfigMapName   string
	saveDeploymentFiles      string
	deploy                   bool
	kubeconfig               string
	userConfig               string
	discoverClusterConfig    bool
	saveClusterConfig        string
	logger                   = log.Log.WithName("l8k")
	enableDocaDriver         bool
	enabledPlugins           string
	networkOperatorNamespace string
	networkOperatorRelease   string
	groups                   []string
	gpuType                  string
	nodeSelector             string
	imagePullSecrets         []string
	networkNamespaces        []string
	outputFormat             string
	yesFlag                  bool
	quietFlag                bool
	workloadManifest         string
	dryRunFlag               bool
	forPreset                string
	deployTimeoutRoot        time.Duration
	keepNamespace            bool
	collapseNicRails         bool
)

// collapseNicRailsFlagHelp is the shared help text for --collapse-nic-rails on
// both the root pipeline and the `discover` subcommand.
const collapseNicRailsFlagHelp = "Advertise one rail per NIC: collapse a NIC's multi-plane PFs to its master PF, keeping a rail per port only for NICs whose VPD model is genuinely dual-port (\"2-port\"/\"Dual-port\"). Set to false to keep the legacy one-rail-per-PF behaviour (dev setups)."

// forFlagHelp is intentionally static. --config-dir is parsed after command
// initialization, so enumerating presets here would always show the default
// source rather than the source selected by the user.
func forFlagHelp() string {
	return "Generate for a known server preset (replaces clusterConfig from the preset). Requires --node-selector. Run 'l8k preset list' with the same --config-dir to list available names."
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "l8k",
	Short: "NVIDIA Kubernetes Launch Kit",
	Long: `
K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

### Discover Cluster Configuration
Deploy a minimal Network Operator profile to automatically discover your cluster's
network capabilities and hardware configuration by using --discover-cluster-config.
This phase can be skipped if you provide your own configuration file by using --user-config.
This phase requires --kubeconfig to be specified.
Discovery fills missing profile settings from the detected hardware and built-in
defaults, applies explicit CLI overrides, and saves the final profile with the
hardware inventory in cluster-config.yaml.

### Generate Deployment Files
Based on the discovered or provided configuration,
generate a complete set of YAML deployment files for the selected network profile.
Files can be saved to disk using --save-deployment-files.
The profile is defined with --fabric, --deployment-type and --multirail flags,
or via a profile section in the user-config file.

### Deploy to Cluster
Apply the generated deployment files to your Kubernetes cluster by using --deploy. This phase requires --kubeconfig and can be skipped if --deploy is not specified.

### AI Agent / Automation Support
Use --output json for structured machine-readable output (single JSON object to stdout).
Use --yes to auto-confirm prompts, --quiet to suppress informational output, and --dry-run to preview deployments.
Use 'l8k schema' to discover tool capabilities programmatically.`,
	Example: `  # Discover cluster and generate SR-IOV ethernet deployment
  l8k --kubeconfig ~/.kube/config --discover-cluster-config \
    --fabric ethernet --deployment-type sriov --save-deployment-files ./output

  # Generate from saved config (no cluster access needed)
  l8k --user-config cluster-config.yaml --fabric ethernet \
    --deployment-type sriov --save-deployment-files ./output

  # Discover + deploy Spectrum-X with JSON output for automation
  l8k --kubeconfig ~/.kube/config --discover-cluster-config \
    --spectrum-x RA2.3 --multiplane-mode hwplb --number-of-planes 4 \
    --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
    --network-operator-release 26.7 --deploy --output json --yes

  # Dry-run: preview what would be deployed
  l8k --user-config cluster-config.yaml --spectrum-x --deploy \
    --dry-run --output json

  # Get tool capabilities as JSON (for AI agents)
  l8k schema`,
	Run: func(cmd *cobra.Command, args []string) {
		// Bare `l8k` invocation: print help instead of erroring on the
		// missing-config validation. Any flag or positional argument
		// signals the user wanted the full-pipeline behaviour.
		if cmd.Flags().NFlag() == 0 && len(args) == 0 {
			_ = cmd.Help()
			return
		}

		enabledPlugins := parseEnabledPlugins(enabledPlugins)
		// Create application options from CLI flags
		opts := options.Options{
			LogLevel:                 logLevel,
			LogFile:                  logFile,
			ConfigDir:                configDir,
			UserConfig:               userConfig,
			DiscoverClusterConfig:    discoverClusterConfig,
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
			NodeSelector:             nodeSelector,
			CollapseNicRails:         collapseNicRails,
			ForPreset:                forPreset,
			ImagePullSecrets:         imagePullSecrets,
			NetworkNamespaces:        networkNamespaces,
			SaveDeploymentFiles:      saveDeploymentFiles,
			Deploy:                   deploy,
			Kubeconfig:               kubeconfig,
			DeployTimeout:            deployTimeoutRoot,
			SaveClusterConfig:        saveClusterConfig,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,
			EnabledPlugins:           enabledPlugins,
			WorkloadManifest:         workloadManifest,
			OutputFormat:             outputFormat,
			Yes:                      yesFlag,
			Quiet:                    quietFlag,
			DryRun:                   dryRunFlag,
		}

		// Set EnableDocaDriver only if the flag was explicitly provided
		if cmd.Flags().Lookup("enable-doca-driver").Changed {
			opts.EnableDocaDriver = &enableDocaDriver
		}

		if err := validateProfileFlagValues(&opts); err != nil {
			exitWithError(apperrors.NewValidationError(err.Error(), err, "Check --spectrum-x flag combinations"), opts.OutputFormat)
		}

		// Validate CLI configuration
		if err := validateConfig(&opts); err != nil {
			exitWithError(apperrors.NewValidationError(err.Error(), err, "Run 'l8k --help' for usage information"), opts.OutputFormat)
		}

		logger.Info("SaveConfig", "val", opts)

		// Create and run the application
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

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Phase 0: Plugin flags
	rootCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")

	// Phase 1: Cluster discovery flags
	rootCmd.Flags().BoolVar(&discoverClusterConfig, "discover-cluster-config", false, "Deploy a thin Network Operator profile to discover cluster capabilities")
	rootCmd.Flags().StringVar(&saveClusterConfig, "save-cluster-config", "", "Save discovered cluster configuration to the specified path (defaults to --user-config path if set, otherwise ./cluster-config.yaml)")
	rootCmd.Flags().StringVar(&userConfig, "user-config", "", "Use provided cluster configuration file (as base config for discovery or as full config without discovery)")
	rootCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override the network operator namespace from the config file")
	rootCmd.Flags().StringVar(&networkOperatorRelease, "network-operator-release", "",
		fmt.Sprintf("Network Operator release line to deploy (MAJOR.MINOR). Selects component image tags + repository from a built-in catalog and drives version-gated template sections. Supported: %s",
			strings.Join(releases.SupportedReleases(), ", ")))

	// Phase 2: Deployment generation flags
	rootCmd.Flags().StringVar(&fabric, "fabric", "", "Select the fabric type to deploy (infiniband, ethernet)")
	rootCmd.Flags().StringVar(&deploymentType, "deployment-type", "", "Select the deployment type (sriov, rdma_shared, host_device)")
	rootCmd.Flags().BoolVar(&multirail, "multirail", false, "Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)")
	rootCmd.Flags().StringVar(&routing, "routing", "", "Secondary-network routing mode: destination-based or source-based. source-based chains the automatic sbr CNI meta-plugin.")
	rootCmd.Flags().BoolVar(&ignoreARP, "ignore-arp", false, "Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails")
	rootCmd.Flags().StringVar(&spectrumXVersion, "spectrum-x", "",
		fmt.Sprintf("Enable Spectrum-X by passing the SPC-X RA version (folds in the legacy --spcx-version). Supported: %v",
			config.SupportedSPCXVersions))
	rootCmd.Flags().StringVar(&multiplaneMode, "multiplane-mode", "", "Spectrum-X multiplane mode: none, swplb, hwplb (requires --spectrum-x)")
	rootCmd.Flags().IntVar(&numberOfPlanes, "number-of-planes", 0, "Number of planes for Spectrum-X (requires --spectrum-x)")
	rootCmd.Flags().StringVar(&topologyScheme, "topology-scheme", "", "Spectrum-X topology scheme for guide-based IP allocation: 2-tier or 3-tier (requires --spectrum-x)")
	rootCmd.Flags().StringVar(&ipVersion, "ip-version", "", "Spectrum-X IP version for guide-based allocation: ipv4 or ipv6 (requires --spectrum-x)")
	rootCmd.Flags().StringVar(&topologyFile, "topology-file", "", "Path to spcx-gen/reference-generator or NVIDIA AIR topology JSON for Spectrum-X CIDRPool generation (requires --spectrum-x)")
	rootCmd.Flags().StringVar(&spectrumXConfig, "spectrum-x-config", "", "Path to full Spectrum-X profile ConfigMap YAML or raw data.profile YAML (required for SPC-X RA versions newer than RA2.2)")
	rootCmd.Flags().StringVar(&spectrumXConfigMapName, "spectrum-x-configmap-name", "", "Spectrum-X profile ConfigMap name when --spectrum-x-config contains raw data.profile YAML")
	rootCmd.Flags().StringSliceVar(&groups, "groups", nil, "Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.")
	rootCmd.Flags().StringVar(&gpuType, "gpu-type", "", "Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.")
	rootCmd.MarkFlagsMutuallyExclusive("groups", "gpu-type")
	rootCmd.Flags().StringVar(&nodeSelector, "node-selector", "feature.node.kubernetes.io/pci-15b3.present=true", "Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe")
	rootCmd.Flags().BoolVar(&collapseNicRails, "collapse-nic-rails", true, collapseNicRailsFlagHelp)
	rootCmd.Flags().StringVar(&forPreset, "for", "", forFlagHelp())
	rootCmd.Flags().StringSliceVar(&imagePullSecrets, "image-pull-secrets", nil, "Image pull secret names for NicClusterPolicy (comma-separated)")
	rootCmd.Flags().StringVar(&saveDeploymentFiles, "save-deployment-files", "./deployment", "Save generated deployment files to the specified directory")
	rootCmd.Flags().StringSliceVar(&networkNamespaces, "network-namespaces", nil, "Comma-separated namespaces for the secondary-network CRs and example test DaemonSets. One independent copy is rendered per namespace (shared resources like IPPools and NodePolicies are NOT duplicated). Overrides config networkNamespaces; default: 'default'.")
	rootCmd.Flags().BoolVar(&enableDocaDriver, "enable-doca-driver", false, "Enable DOCA driver deployment (overrides config file docaDriver.enable)")
	rootCmd.Flags().StringVar(&workloadManifest, "workload-manifest", "", "Path to a custom workload manifest YAML (replaces the profile's default example workload)")

	// Phase 3: Cluster deployment flags
	rootCmd.Flags().BoolVar(&deploy, "deploy", false, "Deploy the generated files to the Kubernetes cluster")
	rootCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file for cluster deployment (required when using --deploy; falls back to $KUBECONFIG, then ~/.kube/config)")
	rootCmd.Flags().DurationVar(&deployTimeoutRoot, "deploy-timeout", 0, "Maximum end-to-end wall-clock budget for the deploy phase (e.g. 45m, 2h). 0 (the default) means no deadline; the deploy polls until every manifest reaches a terminal state.")

	// Output control flags
	rootCmd.PersistentFlags().StringVar(&configDir, "config-dir", "", "Directory containing optional l8k-config.yaml and presets/ overrides")
	rootCmd.PersistentFlags().StringVar(&outputFormat, "output", "text", "Output format: text (default, human-readable) or json (structured, for automation and AI agents)")
	rootCmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Auto-confirm all prompts without interactive input")
	rootCmd.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "Suppress informational output (errors still shown)")
	rootCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Preview what would be deployed without applying changes to the cluster")

	// Logging flags
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "", "Enable logging at specified level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "", "Write logs to file instead of stderr")

	// Group the flags into labelled sections in --help output. The phase
	// comments above describe the same grouping; this propagates the intent
	// into the rendered help. installGroupedUsage propagates the template
	// to every subcommand via cobra's parent lookup.
	setFlagGroup(rootCmd, "enabled-plugins", GroupCommon)
	setFlagGroup(rootCmd, "config-dir", GroupCommon)
	setFlagGroup(rootCmd, "user-config", GroupCommon)
	setFlagGroup(rootCmd, "kubeconfig", GroupCommon)
	setFlagGroup(rootCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(rootCmd, "network-operator-release", GroupCommon)
	setFlagGroup(rootCmd, "node-selector", GroupCommon)
	setFlagGroup(rootCmd, "image-pull-secrets", GroupCommon)

	setFlagGroup(rootCmd, "discover-cluster-config", GroupDiscovery)
	setFlagGroup(rootCmd, "save-cluster-config", GroupDiscovery)

	setFlagGroup(rootCmd, "fabric", GroupProfile)
	setFlagGroup(rootCmd, "deployment-type", GroupProfile)
	setFlagGroup(rootCmd, "multirail", GroupProfile)
	setFlagGroup(rootCmd, "routing", GroupProfile)
	setFlagGroup(rootCmd, "ignore-arp", GroupProfile)
	setFlagGroup(rootCmd, "spectrum-x", GroupProfile)
	setFlagGroup(rootCmd, "groups", GroupProfile)
	setFlagGroup(rootCmd, "gpu-type", GroupProfile)
	setFlagGroup(rootCmd, "for", GroupProfile)

	setFlagGroup(rootCmd, "multiplane-mode", GroupSpectrumX)
	setFlagGroup(rootCmd, "number-of-planes", GroupSpectrumX)
	setFlagGroup(rootCmd, "topology-scheme", GroupSpectrumX)
	setFlagGroup(rootCmd, "ip-version", GroupSpectrumX)
	setFlagGroup(rootCmd, "topology-file", GroupSpectrumX)
	setFlagGroup(rootCmd, "spectrum-x-config", GroupSpectrumX)
	setFlagGroup(rootCmd, "spectrum-x-configmap-name", GroupSpectrumX)

	setFlagGroup(rootCmd, "save-deployment-files", GroupGeneration)
	setFlagGroup(rootCmd, "network-namespaces", GroupGeneration)
	setFlagGroup(rootCmd, "enable-doca-driver", GroupGeneration)
	setFlagGroup(rootCmd, "workload-manifest", GroupGeneration)

	setFlagGroup(rootCmd, "deploy", GroupDeploy)
	setFlagGroup(rootCmd, "deploy-timeout", GroupDeploy)
	setFlagGroup(rootCmd, "dry-run", GroupDeploy)

	setFlagGroup(rootCmd, "output", GroupOutputLogging)
	setFlagGroup(rootCmd, "yes", GroupOutputLogging)
	setFlagGroup(rootCmd, "quiet", GroupOutputLogging)
	setFlagGroup(rootCmd, "log-level", GroupOutputLogging)
	setFlagGroup(rootCmd, "log-file", GroupOutputLogging)

	installGroupedUsage(rootCmd)
}

// validateConfig validates the CLI flag combinations
func validateConfig(options *options.Options) error {
	// Validate --output flag
	if !slices.Contains([]string{"text", "json"}, options.OutputFormat) {
		return fmt.Errorf("--output must be one of: text, json")
	}

	// Validate --network-operator-release against the embedded catalog so the
	// user sees the supported list immediately, before discovery/render.
	if options.NetworkOperatorRelease != "" {
		if _, ok := releases.LookupRelease(options.NetworkOperatorRelease); !ok {
			return fmt.Errorf("unknown --network-operator-release %q; supported: %v",
				options.NetworkOperatorRelease, releases.SupportedReleases())
		}
	}

	// Validate --dry-run requires --deploy
	if options.DryRun && !options.Deploy {
		return fmt.Errorf("--dry-run requires --deploy to be specified (it previews what deploy would do)")
	}

	// Validate --workload-manifest file exists
	if options.WorkloadManifest != "" {
		if _, err := os.Stat(options.WorkloadManifest); os.IsNotExist(err) {
			return fmt.Errorf("workload manifest file does not exist: %s", options.WorkloadManifest)
		}
	}

	// At least one plugin should be enabled
	if len(options.EnabledPlugins) == 0 {
		return fmt.Errorf("no plugins enabled, use --enabled-plugins to enable plugins")
	}

	// Require either a config source, a preset-generated config, or discovery.
	if options.UserConfig == "" && options.ConfigDir == "" && options.ForPreset == "" && !options.DiscoverClusterConfig {
		return fmt.Errorf("one of --user-config, --config-dir, --for, or --discover-cluster-config must be provided")
	}

	// --for synthesizes clusterConfig from a static preset, so it cannot be
	// combined with discovery, and it always needs a node selector to identify
	// which nodes the rendered manifests target.
	if options.ForPreset != "" {
		if options.DiscoverClusterConfig {
			return fmt.Errorf("--for and --discover-cluster-config are mutually exclusive")
		}
		if options.NodeSelector == "" {
			return fmt.Errorf("--for requires --node-selector (specify which nodes the synthesized clusterConfig should target)")
		}
	}

	// Resolve kubeconfig from flag or $KUBECONFIG env var for cluster operations
	if options.DiscoverClusterConfig || options.Deploy {
		resolved, err := resolveKubeconfig(options.Kubeconfig)
		if err != nil {
			return fmt.Errorf("kubeconfig required for cluster operations: set $KUBECONFIG or pass --kubeconfig <path>")
		}
		options.Kubeconfig = resolved
	}

	// Spectrum-X cohort + value validation lives in applySpectrumXDefaults so
	// it runs for both the root command and the `generate` subcommand. Don't
	// duplicate it here — that function is the single authoritative point of
	// rejection for malformed --spectrum-x usage.

	// Network Operator plugin rules
	if slices.Contains(options.EnabledPlugins, networkoperatorplugin.PluginName) {
		// If profile is selected, either save-deployment-files or deploy options should be provided
		if (options.Fabric != "" || options.DeploymentType != "") && options.SaveDeploymentFiles == "" && !options.Deploy {
			return fmt.Errorf("when --deployment-type is specified, either --save-deployment-files or --deploy must be provided")
		}

		// Save-deployment-files or deploy can't work without profile
		if options.Fabric == "" && options.DeploymentType == "" && options.UserConfig == "" && options.Deploy {
			return fmt.Errorf("--deploy requires --deployment-type or --user-config with a profile to be specified")
		}

		if (options.DeploymentType != "" && options.Fabric == "") || (options.Fabric != "" && options.DeploymentType == "") {
			return fmt.Errorf("--deployment-type requires --fabric to be specified")
		}

		if options.Fabric != "" && !slices.Contains([]string{"infiniband", "ethernet"}, options.Fabric) {
			return fmt.Errorf("--fabric must be one of: infiniband, ethernet")
		}

		if options.DeploymentType != "" && !slices.Contains([]string{"sriov", "rdma_shared", "host_device"}, options.DeploymentType) {
			return fmt.Errorf("--deployment-type must be one of: sriov, rdma_shared, host_device")
		}
	}

	return nil
}

// validateProfileFlagValues validates profile enums that can be supplied to
// discover, generate, or the root pipeline before any cluster-side work.
// Partial profiles are valid because the resolution phase fills missing
// values from discovered hardware.
func validateProfileFlagValues(opts *options.Options) error {
	if opts.Fabric != "" && !slices.Contains([]string{"infiniband", "ethernet"}, opts.Fabric) {
		return fmt.Errorf("--fabric must be one of: infiniband, ethernet")
	}
	if opts.DeploymentType != "" && !slices.Contains([]string{"sriov", "rdma_shared", "host_device"}, opts.DeploymentType) {
		return fmt.Errorf("--deployment-type must be one of: sriov, rdma_shared, host_device")
	}
	if opts.Routing != "" && !slices.Contains([]string{config.RoutingDestinationBased, config.RoutingSourceBased}, opts.Routing) {
		return fmt.Errorf("--routing must be one of: %s, %s", config.RoutingDestinationBased, config.RoutingSourceBased)
	}
	return applySpectrumXSyntaxChecks(opts)
}

// applySpectrumXSyntaxChecks is the Phase 1 Spectrum-X enum/value validator. It
// runs in PreRunE BEFORE LoadFullConfig + ApplyHardwareDefaults, so it
// catches obvious typos (e.g. `--multiplane-mode bogus`) up-front
// without false positives from values that defaults are about to fill.
//
// Cohort/cross-flag rules ("RA2.1 requires release 26.1", "spectrum-x
// requires ethernet fabric", etc.) live in
// `pkg/resolve.ValidateResolvedConfig` and run AFTER defaults have a
// chance to fill them in.
//
// Implicit defaulting (Spectrum-X → fabric=ethernet etc.) lives in
// `pkg/resolve.ApplyHardwareDefaults`.
func applySpectrumXSyntaxChecks(opts *options.Options) error {
	// Inverse cohort: --multiplane-mode / --number-of-planes and
	// ConfigMap-backed Spectrum-X profile inputs are only
	// meaningful with --spectrum-x. Catch the CLI typo case here so
	// the user doesn't get a confusing render-time failure.
	if !opts.SpectrumX {
		if opts.MultiplaneMode != "" {
			return fmt.Errorf("--multiplane-mode can only be used with --spectrum-x")
		}
		if opts.NumberOfPlanes != 0 {
			return fmt.Errorf("--number-of-planes can only be used with --spectrum-x")
		}
		if opts.TopologyScheme != "" {
			return fmt.Errorf("--topology-scheme can only be used with --spectrum-x")
		}
		if opts.IPVersion != "" {
			return fmt.Errorf("--ip-version can only be used with --spectrum-x")
		}
		if opts.TopologyFile != "" {
			return fmt.Errorf("--topology-file can only be used with --spectrum-x")
		}
		if opts.SpectrumXConfig != "" {
			return fmt.Errorf("--spectrum-x-config can only be used with --spectrum-x")
		}
		if opts.SpectrumXConfigMapName != "" {
			return fmt.Errorf("--spectrum-x-configmap-name can only be used with --spectrum-x")
		}
		return nil
	}

	// SPCXVersion enum check — defensive even though Run() should
	// have set SpectrumX only when SPCXVersion is non-empty.
	if opts.SPCXVersion == "" {
		return fmt.Errorf("--spectrum-x requires the SPC-X RA version as its value; supported: %v",
			config.SupportedSPCXVersions)
	}
	if !slices.Contains(config.SupportedSPCXVersions, opts.SPCXVersion) {
		return fmt.Errorf("invalid --spectrum-x value %q; supported: %v",
			opts.SPCXVersion, config.SupportedSPCXVersions)
	}

	// Enum checks for --multiplane-mode and --number-of-planes only
	// fire when the user supplied a value; defaults fill them later.
	if opts.MultiplaneMode != "" && !slices.Contains(config.SupportedMultiplaneModes, opts.MultiplaneMode) {
		return fmt.Errorf("invalid --multiplane-mode %q; supported: %v",
			opts.MultiplaneMode, config.SupportedMultiplaneModes)
	}
	if opts.NumberOfPlanes != 0 && !slices.Contains(config.SupportedNumberOfPlanes, opts.NumberOfPlanes) {
		return fmt.Errorf("invalid --number-of-planes %d; supported: %v",
			opts.NumberOfPlanes, config.SupportedNumberOfPlanes)
	}
	if opts.TopologyScheme != "" && !slices.Contains(config.SupportedSpectrumXTopologyTypes, opts.TopologyScheme) {
		return fmt.Errorf("invalid --topology-scheme %q; supported: %v",
			opts.TopologyScheme, config.SupportedSpectrumXTopologyTypes)
	}
	if opts.IPVersion != "" && !slices.Contains(config.SupportedSpectrumXIPVersions, opts.IPVersion) {
		return fmt.Errorf("invalid --ip-version %q; supported: %v",
			opts.IPVersion, config.SupportedSpectrumXIPVersions)
	}

	// --network-operator-release enum check (when supplied). The
	// (RA, release) pairing is enforced in Phase 2 against the
	// resolved cfg, but we can reject obvious typos here — the
	// release line is consequential (CRD shape + SR-IOV operator
	// behaviour), so a misspelled release is worth catching early.
	if opts.NetworkOperatorRelease != "" {
		allowed := config.SPCXVersionAllowedReleases[opts.SPCXVersion]
		if !slices.Contains(allowed, opts.NetworkOperatorRelease) {
			return fmt.Errorf("--spectrum-x %s requires --network-operator-release in %v, got %s",
				opts.SPCXVersion, allowed, opts.NetworkOperatorRelease)
		}
	}

	return nil
}

// exitWithError prints the error and exits with the appropriate code.
// In JSON mode, emits a structured JSON error to stdout. In text mode,
// prints `Error: <message>` followed by `Suggestion: <suggestion>` on
// a separate line when the structured error carries one — surfaces the
// remediation hint without redundancy when the message already mentions
// the wrong state.
func exitWithError(se *apperrors.StructuredError, outputFormat string) {
	if outputFormat == "json" {
		errJSON, _ := json.Marshal(se)
		result := ui.JSONResult{
			Success: false,
			Error:   errJSON,
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintf(os.Stdout, "%s\n", data)
		fmt.Fprintf(os.Stderr, "Error: %s\n", se.Error())
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", se.Error())
		if se.Suggestion != "" {
			fmt.Fprintf(os.Stderr, "Suggestion: %s\n", se.Suggestion)
		}
	}
	os.Exit(se.ExitCode)
}

func parseEnabledPlugins(enabledPlugins string) []string {
	return strings.Split(enabledPlugins, ",")
}

// defaultKubeconfigHomeFile is the kubectl-style default kubeconfig location
// ($HOME/.kube/config). Overridable in tests.
var defaultKubeconfigHomeFile = clientcmd.RecommendedHomeFile

// resolveKubeconfig resolves the kubeconfig path from flag or environment.
// Priority: 1) --kubeconfig flag, 2) $KUBECONFIG env var, 3) $HOME/.kube/config
// (the kubectl default — only returned when the file exists).
func resolveKubeconfig(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envVal := os.Getenv("KUBECONFIG"); envVal != "" {
		return envVal, nil
	}
	if defaultKubeconfigHomeFile != "" {
		if _, err := os.Stat(defaultKubeconfigHomeFile); err == nil {
			return defaultKubeconfigHomeFile, nil
		}
	}
	return "", fmt.Errorf("no kubeconfig found: set $KUBECONFIG, pass --kubeconfig <path>, or create %s", defaultKubeconfigHomeFile)
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	// Detect if --log-level was explicitly set
	logLevelFlag := rootCmd.PersistentFlags().Lookup("log-level")
	loggingEnabled := logLevelFlag.Changed

	if loggingEnabled {
		applog.SetLoggingEnabled(true)
		if logLevel == "" {
			logLevel = "info" // default when enabled
		}
	} else {
		applog.SetLoggingEnabled(false)
	}

	// Configure log file if specified
	if logFile != "" {
		if err := applog.SetLogFile(logFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to open log file: %v\n", err)
		}
	}

	// Initialize logging
	applog.InitLog()

	// Set log level if logging is enabled
	if loggingEnabled && logLevel != "" {
		if err := applog.SetLogLevel(logLevel); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Invalid log level %q: %v\n", logLevel, err)
		}
	}

	// Implementation for config initialization
	// This can be expanded later to read from config files
}
