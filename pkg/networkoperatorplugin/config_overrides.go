// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ConfigFlagMapping describes the configuration fields controlled by one CLI
// flag. A flag may update more than one field when it selects a coordinated
// configuration cohort, as --network-operator-release does.
type ConfigFlagMapping struct {
	FlagName    string
	ConfigPaths []string
}

type configFlagOverride struct {
	ConfigFlagMapping
	shouldApply func(options.Options, *config.LaunchKitConfig) bool
	apply       func(options.Options, *config.LaunchKitConfig) error
}

// ConfigFlagMappings returns the canonical CLI-to-config mapping without the
// internal setter functions. Callers use this for schema/help introspection;
// ApplyCLIConfigOverrides is the only mutation entry point.
func ConfigFlagMappings() []ConfigFlagMapping {
	registry := configFlagOverrideRegistry()
	out := make([]ConfigFlagMapping, 0, len(registry))
	for _, entry := range registry {
		out = append(out, ConfigFlagMapping{
			FlagName:    entry.FlagName,
			ConfigPaths: append([]string(nil), entry.ConfigPaths...),
		})
	}
	return out
}

// ConfigPathsForFlag returns the config paths controlled by flagName. Both
// "flag-name" and "--flag-name" forms are accepted.
func ConfigPathsForFlag(flagName string) []string {
	flagName = strings.TrimPrefix(flagName, "--")
	for _, mapping := range configFlagOverrideRegistry() {
		if mapping.FlagName == flagName {
			return append([]string(nil), mapping.ConfigPaths...)
		}
	}
	return nil
}

// ApplyCLIConfigOverrides is the single mutation boundary for config-backed
// CLI flags. It is shared by discover, generate, standalone deploy, and
// standalone validate so a new mapping is implemented once.
func ApplyCLIConfigOverrides(opts options.Options, cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}

	for _, entry := range configFlagOverrideRegistry() {
		if !entry.shouldApply(opts, cfg) {
			continue
		}
		if err := entry.apply(opts, cfg); err != nil {
			return fmt.Errorf("apply --%s to %s: %w",
				entry.FlagName, strings.Join(entry.ConfigPaths, ", "), err)
		}
		log.Log.V(1).Info("Applied config flag mapping",
			"flag", entry.FlagName, "configPaths", entry.ConfigPaths)
	}

	if opts.SpectrumX && cfg.Profile != nil && cfg.Profile.SpectrumX != nil {
		if err := config.NormalizeSpectrumXProfileConfig(cfg.Profile.SpectrumX); err != nil {
			return fmt.Errorf("invalid Spectrum-X profile config: %w", err)
		}
	}
	return nil
}

func configFlagOverrideRegistry() []configFlagOverride {
	profileAvailable := func(_ options.Options, cfg *config.LaunchKitConfig) bool {
		return cfg.Profile != nil
	}
	spectrumXAvailable := func(opts options.Options, cfg *config.LaunchKitConfig) bool {
		return opts.SpectrumX && cfg.Profile != nil
	}

	return []configFlagOverride{
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName: "network-operator-release",
				ConfigPaths: []string{
					"networkOperator.selectedRelease",
					"networkOperator.version",
					"networkOperator.componentVersion",
					"networkOperator.repository",
					"networkOperator.operatorRepository",
					"networkOperator.helmRepoURL",
					"docaDriver.version",
				},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return opts.NetworkOperatorRelease != "" ||
					(cfg.NetworkOperator != nil && cfg.NetworkOperator.SelectedRelease != "")
			},
			apply: ApplyNetworkOperatorRelease,
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "network-operator-namespace",
				ConfigPaths: []string{"networkOperator.namespace"},
			},
			shouldApply: func(opts options.Options, _ *config.LaunchKitConfig) bool {
				return opts.NetworkOperatorNamespace != ""
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				ensureNetworkOperatorConfig(cfg).Namespace = opts.NetworkOperatorNamespace
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "skip-network-operator-helm",
				ConfigPaths: []string{"networkOperator.skipHelmChart"},
			},
			shouldApply: func(opts options.Options, _ *config.LaunchKitConfig) bool {
				return opts.SkipNetworkOperatorHelmSet
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				ensureNetworkOperatorConfig(cfg).SkipHelmChart = opts.SkipNetworkOperatorHelm
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "image-pull-secrets",
				ConfigPaths: []string{"networkOperator.imagePullSecrets"},
			},
			shouldApply: func(opts options.Options, _ *config.LaunchKitConfig) bool {
				return len(opts.ImagePullSecrets) > 0
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				ensureNetworkOperatorConfig(cfg).ImagePullSecrets = append([]string(nil), opts.ImagePullSecrets...)
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "network-namespaces",
				ConfigPaths: []string{"networkNamespaces"},
			},
			shouldApply: func(opts options.Options, _ *config.LaunchKitConfig) bool {
				return len(opts.NetworkNamespaces) > 0
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				cfg.NetworkNamespaces = append([]string(nil), opts.NetworkNamespaces...)
				return nil
			},
		},
		stringProfileOverride("fabric", "profile.fabric", profileAvailable,
			func(opts options.Options) string { return opts.Fabric },
			func(profile *config.Profile, value string) { profile.Fabric = value }),
		stringProfileOverride("deployment-type", "profile.deployment", profileAvailable,
			func(opts options.Options) string { return opts.DeploymentType },
			func(profile *config.Profile, value string) { profile.Deployment = value }),
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "multirail",
				ConfigPaths: []string{"profile.multirail"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return opts.MultirailSet && profileAvailable(opts, cfg)
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				cfg.Profile.Multirail = opts.Multirail
				cfg.Profile.MultirailSet = true
				return nil
			},
		},
		stringProfileOverride("routing", "profile.routing", profileAvailable,
			func(opts options.Options) string { return opts.Routing },
			func(profile *config.Profile, value string) { profile.Routing = value }),
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "ignore-arp",
				ConfigPaths: []string{"profile.ignoreARP"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return opts.IgnoreARPSet && profileAvailable(opts, cfg)
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				cfg.Profile.IgnoreARP = opts.IgnoreARP
				cfg.Profile.IgnoreARPSet = true
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "workload-manifest",
				ConfigPaths: []string{"workload.manifest"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return opts.WorkloadManifest != "" && profileAvailable(opts, cfg)
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				if cfg.Workload == nil {
					cfg.Workload = &config.WorkloadConfig{}
				}
				cfg.Workload.Manifest = opts.WorkloadManifest
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "enable-doca-driver",
				ConfigPaths: []string{"docaDriver.enable"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return opts.EnableDocaDriver != nil && profileAvailable(opts, cfg)
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				ensureDOCADriverConfig(cfg).Enable = *opts.EnableDocaDriver
				return nil
			},
		},
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "spectrum-x",
				ConfigPaths: []string{"profile.spectrumX.enable", "profile.spectrumX.spcxVersion"},
			},
			shouldApply: spectrumXAvailable,
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				sx := ensureSpectrumXProfile(cfg)
				sx.Enable = true
				if opts.SPCXVersion != "" {
					sx.SPCXVersion = opts.SPCXVersion
				}
				return nil
			},
		},
		spectrumXStringOverride("multiplane-mode", "profile.spectrumX.multiplaneMode", spectrumXAvailable,
			func(opts options.Options) string { return opts.MultiplaneMode },
			func(sx *config.ProfileSpectrumX, value string) { sx.MultiplaneMode = value }),
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "number-of-planes",
				ConfigPaths: []string{"profile.spectrumX.numberOfPlanes"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return spectrumXAvailable(opts, cfg) && opts.NumberOfPlanes != 0
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				ensureSpectrumXProfile(cfg).NumberOfPlanes = opts.NumberOfPlanes
				return nil
			},
		},
		spectrumXStringOverride("topology-scheme", "profile.spectrumX.topologyType", spectrumXAvailable,
			func(opts options.Options) string { return opts.TopologyScheme },
			func(sx *config.ProfileSpectrumX, value string) { sx.TopologyType = value }),
		spectrumXStringOverride("ip-version", "profile.spectrumX.ipVersion", spectrumXAvailable,
			func(opts options.Options) string { return opts.IPVersion },
			func(sx *config.ProfileSpectrumX, value string) { sx.IPVersion = value }),
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "topology-file",
				ConfigPaths: []string{"profile.spectrumX.topologyFile"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return spectrumXAvailable(opts, cfg) && opts.TopologyFile != ""
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				resolved, err := filepath.Abs(opts.TopologyFile)
				if err != nil {
					return fmt.Errorf("resolve path %s: %w", opts.TopologyFile, err)
				}
				sx := ensureSpectrumXProfile(cfg)
				sx.TopologyFile = opts.TopologyFile
				sx.ResolvedTopologyFile = resolved
				return nil
			},
		},
		spectrumXStringOverride("spectrum-x-configmap-name", "profile.spectrumX.configMapName", spectrumXAvailable,
			func(opts options.Options) string { return opts.SpectrumXConfigMapName },
			func(sx *config.ProfileSpectrumX, value string) { sx.ConfigMapName = value }),
		{
			ConfigFlagMapping: ConfigFlagMapping{
				FlagName:    "spectrum-x-config",
				ConfigPaths: []string{"profile.spectrumX.profile"},
			},
			shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
				return spectrumXAvailable(opts, cfg) && opts.SpectrumXConfig != ""
			},
			apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
				profileConfig, err := os.ReadFile(opts.SpectrumXConfig)
				if err != nil {
					return fmt.Errorf("read %s: %w", opts.SpectrumXConfig, err)
				}
				ensureSpectrumXProfile(cfg).Profile = string(profileConfig)
				return nil
			},
		},
	}
}

func stringProfileOverride(
	flagName string,
	configPath string,
	profileAvailable func(options.Options, *config.LaunchKitConfig) bool,
	value func(options.Options) string,
	set func(*config.Profile, string),
) configFlagOverride {
	return configFlagOverride{
		ConfigFlagMapping: ConfigFlagMapping{FlagName: flagName, ConfigPaths: []string{configPath}},
		shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
			return profileAvailable(opts, cfg) && value(opts) != ""
		},
		apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
			set(cfg.Profile, value(opts))
			return nil
		},
	}
}

func spectrumXStringOverride(
	flagName string,
	configPath string,
	spectrumXAvailable func(options.Options, *config.LaunchKitConfig) bool,
	value func(options.Options) string,
	set func(*config.ProfileSpectrumX, string),
) configFlagOverride {
	return configFlagOverride{
		ConfigFlagMapping: ConfigFlagMapping{FlagName: flagName, ConfigPaths: []string{configPath}},
		shouldApply: func(opts options.Options, cfg *config.LaunchKitConfig) bool {
			return spectrumXAvailable(opts, cfg) && value(opts) != ""
		},
		apply: func(opts options.Options, cfg *config.LaunchKitConfig) error {
			set(ensureSpectrumXProfile(cfg), value(opts))
			return nil
		},
	}
}

func ensureNetworkOperatorConfig(cfg *config.LaunchKitConfig) *config.NetworkOperatorConfig {
	if cfg.NetworkOperator == nil {
		cfg.NetworkOperator = &config.NetworkOperatorConfig{}
	}
	return cfg.NetworkOperator
}

func ensureDOCADriverConfig(cfg *config.LaunchKitConfig) *config.DOCADriverConfig {
	if cfg.DOCADriver == nil {
		cfg.DOCADriver = &config.DOCADriverConfig{
			UnloadStorageModules:        true,
			UnloadThirdPartyRDMAModules: true,
			SkipPreflightChecks:         false,
		}
	}
	return cfg.DOCADriver
}

func ensureSpectrumXProfile(cfg *config.LaunchKitConfig) *config.ProfileSpectrumX {
	if cfg.Profile.SpectrumX == nil {
		cfg.Profile.SpectrumX = &config.ProfileSpectrumX{}
	}
	return cfg.Profile.SpectrumX
}
