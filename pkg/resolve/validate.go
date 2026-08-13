// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// ValidateResolvedConfig is the Phase 2 cohort validator. It runs
// AFTER `ApplyHardwareDefaults` + `ApplyOptionsToConfig`, against the
// fully-resolved cfg, so it can enforce cross-flag relationships
// without false positives from values that defaults are about to fill.
//
// Examples of rules that live here (vs. Phase 1 enum sanity in
// `pkg/cmd/applySpectrumXSyntaxChecks`):
//
//   - "Spectrum-X requires fabric=ethernet"
//   - "Spectrum-X requires deployment=sriov"
//   - "Spectrum-X RA2.1 requires --network-operator-release in [26.1]"
//   - "multiplane-mode=none requires number-of-planes=1"
//   - "Spectrum-X needs --multiplane-mode, --number-of-planes, and
//     --network-operator-release set" (after defaults filled what hardware could)
//
// Returns a single error describing the first violation. Caller wraps
// this in a structured ValidationError for the user.
func ValidateResolvedConfig(cfg *config.LaunchKitConfig) error {
	if cfg == nil || cfg.Profile == nil {
		return nil
	}
	if err := validateRouting(cfg.Profile); err != nil {
		return err
	}

	if cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable {
		return validateSpectrumXCohort(cfg)
	}

	// Inverse cohort: the Spectrum-X-only fields must not have leaked
	// into a non-Spectrum-X profile (a config-file mistake or an
	// orphaned --multiplane-mode flag without --spectrum-x — Phase 1
	// catches the CLI case but a YAML user could still trip this).
	if cfg.Profile.SpectrumX != nil {
		if cfg.Profile.SpectrumX.MultiplaneMode != "" {
			return fmt.Errorf("multiplaneMode is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.NumberOfPlanes != 0 {
			return fmt.Errorf("numberOfPlanes is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.TopologyType != "" {
			return fmt.Errorf("topologyType is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.IPVersion != "" {
			return fmt.Errorf("ipVersion is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.TopologyFile != "" {
			return fmt.Errorf("topologyFile is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.SPCXVersion != "" {
			return fmt.Errorf("spcxVersion is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.Profile != "" {
			return fmt.Errorf("profile is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
		if cfg.Profile.SpectrumX.ConfigMapName != "" {
			return fmt.Errorf("configMapName is set but spectrumX.enable=false; remove the field or set --spectrum-x")
		}
	}

	return nil
}

func validateRouting(profile *config.Profile) error {
	switch profile.Routing {
	case "", config.RoutingDestinationBased, config.RoutingSourceBased:
	default:
		return fmt.Errorf("profile.routing must be one of: %s, %s",
			config.RoutingDestinationBased, config.RoutingSourceBased)
	}
	return nil
}

// validateSpectrumXCohort enforces cross-flag rules for Spectrum-X
// profiles after hardware defaults + CLI overlay. Same checks that
// `pkg/cmd/applySpectrumXDefaults` ran, just sourced from cfg
// (resolved values) rather than opts (raw CLI input).
func validateSpectrumXCohort(cfg *config.LaunchKitConfig) error {
	spcx := cfg.Profile.SpectrumX

	// Spectrum-X always implies ethernet fabric + sriov deployment.
	if cfg.Profile.Fabric != "" && cfg.Profile.Fabric != "ethernet" {
		return fmt.Errorf("--spectrum-x requires ethernet fabric, got %q", cfg.Profile.Fabric)
	}
	if cfg.Profile.Deployment != "" && cfg.Profile.Deployment != "sriov" {
		return fmt.Errorf("--spectrum-x requires sriov deployment, got %q", cfg.Profile.Deployment)
	}
	if !cfg.Profile.Multirail {
		return fmt.Errorf("--spectrum-x requires multirail=true")
	}
	if cfg.Profile.Routing != "" && cfg.Profile.Routing != config.RoutingDestinationBased {
		return fmt.Errorf("--routing does not apply to Spectrum-X profiles; use %s or remove the field",
			config.RoutingDestinationBased)
	}
	if cfg.Profile.IgnoreARP {
		return fmt.Errorf("--ignore-arp does not apply to Spectrum-X profiles; remove the flag or set ignoreARP: false")
	}

	// SPCXVersion must be set and registered (Phase 1 enforces the
	// allowed-set, but a config-file user could bypass that).
	if spcx.SPCXVersion == "" {
		return fmt.Errorf("--spectrum-x requires the SPC-X RA version; supported: %v", config.SupportedSPCXVersions)
	}
	if !slices.Contains(config.SupportedSPCXVersions, spcx.SPCXVersion) {
		return fmt.Errorf("invalid SPC-X RA version %q; supported: %v", spcx.SPCXVersion, config.SupportedSPCXVersions)
	}
	if config.SpectrumXProfileConfigRequired(spcx.SPCXVersion) {
		if strings.TrimSpace(spcx.Profile) == "" {
			return fmt.Errorf("--spectrum-x %s requires a Spectrum-X profile ConfigMap input via --spectrum-x-config or profile.spectrumX.profile",
				spcx.SPCXVersion)
		}
		if strings.TrimSpace(spcx.ConfigMapName) == "" {
			return fmt.Errorf("--spectrum-x %s requires a Spectrum-X profile ConfigMap name via --spectrum-x-configmap-name or profile.spectrumX.configMapName when the input is raw data.profile YAML",
				spcx.SPCXVersion)
		}
	}

	// MultiplaneMode + NumberOfPlanes — these can come from
	// ApplyHardwareDefaults; if BOTH defaulting AND the user failed to
	// supply them, we land here with empty values and the user gets a
	// clean cohort error instead of a render-time crash.
	if spcx.MultiplaneMode == "" {
		return fmt.Errorf("--multiplane-mode is required when --spectrum-x is set; supported: %v",
			config.SupportedMultiplaneModes)
	}
	if !slices.Contains(config.SupportedMultiplaneModes, spcx.MultiplaneMode) {
		return fmt.Errorf("invalid --multiplane-mode %q; supported: %v",
			spcx.MultiplaneMode, config.SupportedMultiplaneModes)
	}
	if spcx.NumberOfPlanes == 0 {
		return fmt.Errorf("--number-of-planes is required when --spectrum-x is set; supported: %v",
			config.SupportedNumberOfPlanes)
	}
	if !slices.Contains(config.SupportedNumberOfPlanes, spcx.NumberOfPlanes) {
		return fmt.Errorf("invalid --number-of-planes %d; supported: %v",
			spcx.NumberOfPlanes, config.SupportedNumberOfPlanes)
	}
	if spcx.TopologyType == "" {
		return fmt.Errorf("--topology-scheme is required when --spectrum-x is set; supported: %v",
			config.SupportedSpectrumXTopologyTypes)
	}
	if !slices.Contains(config.SupportedSpectrumXTopologyTypes, spcx.TopologyType) {
		return fmt.Errorf("invalid --topology-scheme %q; supported: %v",
			spcx.TopologyType, config.SupportedSpectrumXTopologyTypes)
	}
	if spcx.IPVersion == "" {
		spcx.IPVersion = config.SpectrumXIPVersionIPv4
	}
	if !slices.Contains(config.SupportedSpectrumXIPVersions, spcx.IPVersion) {
		return fmt.Errorf("invalid --ip-version %q; supported: %v",
			spcx.IPVersion, config.SupportedSpectrumXIPVersions)
	}

	// Cross-validate mode ↔ planes. "none" is exactly one plane; software
	// and hardware plane load balancing require a multiplane count.
	if spcx.MultiplaneMode == "none" && spcx.NumberOfPlanes != 1 {
		return fmt.Errorf("--multiplane-mode none requires --number-of-planes 1, got %d", spcx.NumberOfPlanes)
	}
	if spcx.MultiplaneMode != "none" && spcx.NumberOfPlanes == 1 {
		return fmt.Errorf("--multiplane-mode %s requires --number-of-planes 2 or 4, got 1", spcx.MultiplaneMode)
	}

	// Cross-validate (RA version, network-operator-release).
	allowed := config.SPCXVersionAllowedReleases[spcx.SPCXVersion]
	currentRelease := ""
	if cfg.NetworkOperator != nil {
		currentRelease = cfg.NetworkOperator.SelectedRelease
	}
	if currentRelease == "" {
		return fmt.Errorf("--network-operator-release is required when --spectrum-x is set; "+
			"--spectrum-x %s requires --network-operator-release in %v",
			spcx.SPCXVersion, allowed)
	}
	if !slices.Contains(allowed, currentRelease) {
		return fmt.Errorf("--spectrum-x %s requires --network-operator-release in %v, got %s",
			spcx.SPCXVersion, allowed, currentRelease)
	}

	return nil
}
