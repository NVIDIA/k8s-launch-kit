// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// ApplyCLIConfigOverrides applies explicit values and expands a release
// selected by YAML when the CLI did not replace it.
func ApplyCLIConfigOverrides(opts options.Options, cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	inputs := configflags.Infer(opts)
	if err := ApplyExplicitConfigInputs(inputs, cfg); err != nil {
		return err
	}
	if !hasRequest(inputs.Requests, "network-operator-release") &&
		cfg.NetworkOperator != nil && cfg.NetworkOperator.SelectedRelease != "" {
		if err := ExpandNetworkOperatorRelease(cfg.NetworkOperator.SelectedRelease, cfg); err != nil {
			return fmt.Errorf("expand networkOperator.selectedRelease: %w", err)
		}
	}
	return normalizeExplicitSpectrumXConfig(cfg)
}

// ApplyExplicitCLIConfigOverrides applies only explicitly supplied CLI values.
func ApplyExplicitCLIConfigOverrides(opts options.Options, cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if err := ApplyExplicitConfigInputs(configflags.Infer(opts), cfg); err != nil {
		return err
	}
	return normalizeExplicitSpectrumXConfig(cfg)
}

// ApplyExplicitConfigInputs applies direct mappings generically and delegates
// coordinated flags to typed domain handlers.
func ApplyExplicitConfigInputs(inputs configinput.Values, cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if err := config.ApplyOverrides(cfg, inputs.Overrides); err != nil {
		return err
	}
	for _, override := range inputs.Overrides {
		if override.Path != "profile.spectrumX.topologyFile" {
			continue
		}
		path, ok := override.Value.(string)
		if !ok || path == "" {
			continue
		}
		resolved, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve --%s path %s: %w", override.Flag, path, err)
		}
		ensureSpectrumXProfile(cfg).ResolvedTopologyFile = resolved
	}
	for _, request := range inputs.Requests {
		if err := applyConfigRequest(request, cfg); err != nil {
			return fmt.Errorf("apply --%s to %s: %w",
				request.Flag, strings.Join(configflags.ConfigPathsForFlag(request.Flag), ", "), err)
		}
	}
	return nil
}

// ExpandNetworkOperatorRelease makes the selected catalog entry authoritative
// for every catalog-managed coordinate.
func ExpandNetworkOperatorRelease(release string, cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if release == "" {
		return nil
	}
	releaseEntry, ok := releases.LookupRelease(release)
	if !ok {
		return fmt.Errorf("unsupported network operator release %q; supported: %v",
			release, releases.SupportedReleases())
	}
	if cfg.NetworkOperator == nil {
		cfg.NetworkOperator = &config.NetworkOperatorConfig{}
	}
	cfg.NetworkOperator.SelectedRelease = release
	cfg.NetworkOperator.Version = releaseEntry.NetworkOperator.Version
	cfg.NetworkOperator.ComponentVersion = releaseEntry.NetworkOperator.ComponentVersion
	cfg.NetworkOperator.Repository = releaseEntry.NetworkOperator.Repository
	cfg.NetworkOperator.OperatorRepository = releaseEntry.NetworkOperator.OperatorRepository
	cfg.NetworkOperator.HelmRepoURL = releaseEntry.NetworkOperator.HelmRepoURL
	if cfg.DOCADriver == nil {
		cfg.DOCADriver = &config.DOCADriverConfig{
			UnloadStorageModules:        true,
			UnloadThirdPartyRDMAModules: true,
			SkipPreflightChecks:         false,
		}
	}
	cfg.DOCADriver.Version = releaseEntry.DOCADriver.Version
	return nil
}

func applyConfigRequest(request configinput.Request, cfg *config.LaunchKitConfig) error {
	switch request.Kind {
	case configinput.ResolverNetworkOperatorRelease:
		release, ok := request.Value.(string)
		if !ok {
			return fmt.Errorf("expected string release, got %T", request.Value)
		}
		if release == "" {
			return fmt.Errorf("release must not be empty")
		}
		return ExpandNetworkOperatorRelease(release, cfg)
	case configinput.ResolverSpectrumX:
		version, ok := request.Value.(string)
		if !ok {
			return fmt.Errorf("expected string RA version, got %T", request.Value)
		}
		sx := ensureSpectrumXProfile(cfg)
		sx.Enable = true
		// Programmatic callers historically used SpectrumX=true with an empty
		// SPCXVersion to enable an already configured cohort. The CLI rejects an
		// explicitly empty --spectrum-x before resolution.
		if version != "" {
			sx.SPCXVersion = version
		}
		return nil
	case configinput.ResolverSpectrumXConfig:
		path, ok := request.Value.(string)
		if !ok {
			return fmt.Errorf("expected string path, got %T", request.Value)
		}
		profile, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		ensureSpectrumXProfile(cfg).Profile = string(profile)
		return nil
	default:
		return fmt.Errorf("unknown config resolver %q", request.Kind)
	}
}

func normalizeExplicitSpectrumXConfig(cfg *config.LaunchKitConfig) error {
	if cfg.Profile == nil || cfg.Profile.SpectrumX == nil {
		return nil
	}
	if err := config.NormalizeSpectrumXProfileConfig(cfg.Profile.SpectrumX); err != nil {
		return fmt.Errorf("invalid Spectrum-X profile config: %w", err)
	}
	return nil
}

func ensureSpectrumXProfile(cfg *config.LaunchKitConfig) *config.ProfileSpectrumX {
	if cfg.Profile == nil {
		cfg.Profile = &config.Profile{}
	}
	if cfg.Profile.SpectrumX == nil {
		cfg.Profile.SpectrumX = &config.ProfileSpectrumX{}
	}
	return cfg.Profile.SpectrumX
}

func hasRequest(requests []configinput.Request, kind string) bool {
	for _, request := range requests {
		if request.Kind == kind {
			return true
		}
	}
	return false
}
