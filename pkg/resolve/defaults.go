// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package resolve fills profile-related fields on a loaded
// `LaunchKitConfig` from discovered hardware (defaults) and then
// validates the fully-resolved configuration's cohort rules. The two
// halves are intentionally separate functions so the launcher can wire
// them in around `ApplyOptionsToConfig`:
//
//	LoadFullConfig (cfg.Profile populated from YAML)
//	→ ApplyHardwareDefaults  (fills empty fields from cluster hardware)
//	→ ApplyOptionsToConfig   (CLI flags overlay; non-zero values win)
//	→ ValidateResolvedConfig (cohort + cross-flag checks on resolved cfg)
//
// Precedence (lowest to highest): hardware default < config-file <
// CLI flag. `ApplyHardwareDefaults` checks "is cfg.X already set?"
// before writing, so config-file values survive. Bool flags use
// `Profile.MultirailSet` and `Options.MultirailSet` to distinguish
// omitted values from explicit false values in YAML and on the CLI.
package resolve

import (
	"fmt"
	"strings"
	"unicode"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// DefaultDecision is the audit-trail entry for one applied hardware
// default. Caller logs at info level so each defaulted flag is visible
// to the user without `--log-level debug`.
type DefaultDecision struct {
	Flag   string
	Value  string
	Reason string
}

// String formats the decision for the info-level summary line.
func (d DefaultDecision) String() string {
	return fmt.Sprintf("Defaulted %s=%s (%s)", d.Flag, d.Value, d.Reason)
}

// ApplyHardwareDefaults fills empty profile fields with values derived
// from the discovered cluster (cfg.ClusterConfig) and from the
// already-set CLI flags (opts). Returns the audit trail for every
// applied default.
//
// Defaults applied:
//
//	--fabric              ← unanimous group LinkType (Unit 5). Skipped+warned
//	                       when groups disagree or any group has empty LinkType.
//	--deployment-type     ← "sriov" (always).
//	--multirail           ← true (unless config or CLI explicitly sets it).
//	--routing             ← "destination-based" (unless config or CLI sets it).
//	--multiplane-mode     ← per GPU platform + east-west PF deviceID
//	                       (only when --spectrum-x):
//	                       H100/H200/B200/GB200 → "none"
//	                       B300/GB300 → "swplb" (the GA default;
//	                       hwplb remains an explicit opt-in)
//	                       Unknown platforms fall back to the NIC family.
//	                       Skipped+warned when groups need different defaults.
//	--number-of-planes    ← per platform + deviceID (only when
//	                       --spectrum-x): single-plane platforms → 1;
//	                       B300/GB300 → 2 (pass 4 explicitly for a
//	                       quad-plane B300 topology); CX9 fallback → 4.
//	--network-operator-release ← `config.DefaultSPCXReleaseFor(SPCXVersion)`
//	                            (only when --spectrum-x is set).
//
// `--spectrum-x` itself is NOT defaulted — the user always specifies the
// RA version (per design discussion).
func ApplyHardwareDefaults(cfg *config.LaunchKitConfig, opts options.Options) []DefaultDecision {
	if cfg.Profile == nil {
		cfg.Profile = &config.Profile{}
	}
	var decisions []DefaultDecision
	cfgHasSpectrumX := cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable

	// --fabric ----------------------------------------------------------
	// Spectrum-X forces ethernet fabric; skip hardware linkType default
	// when active so the Spectrum-X branch can default it to ethernet.
	if cfg.Profile.Fabric == "" && opts.Fabric == "" && !opts.SpectrumX && !cfgHasSpectrumX {
		fabric, ok, reason := dominantLinkType(cfg.ClusterConfig)
		log.Log.V(1).Info("HW default: --fabric",
			"current", cfg.Profile.Fabric, "cliValue", opts.Fabric,
			"groupsConsidered", len(cfg.ClusterConfig),
			"resolvedTo", fabric, "applied", ok, "reason", reason)
		if ok {
			cfg.Profile.Fabric = fabric
			decisions = append(decisions, DefaultDecision{
				Flag: "--fabric", Value: fabric,
				Reason: "linkType unanimous across groups",
			})
		} else {
			log.Log.Info("Cannot default --fabric", "reason", reason)
		}
	}

	// --deployment-type -------------------------------------------------
	if cfg.Profile.Deployment == "" && opts.DeploymentType == "" {
		cfg.Profile.Deployment = "sriov"
		decisions = append(decisions, DefaultDecision{
			Flag: "--deployment-type", Value: "sriov", Reason: "default",
		})
		log.Log.V(1).Info("HW default: --deployment-type=sriov (default)")
	}

	// --multirail -------------------------------------------------------
	// Bool: skip the default when either the YAML field or CLI flag was
	// explicitly set (to true or false). The two presence markers keep an
	// explicit false stable across discover/generate round trips.
	if !cfg.Profile.Multirail && !cfg.Profile.MultirailSet && !opts.MultirailSet {
		reason := "default"
		if opts.SpectrumX || cfgHasSpectrumX {
			reason = "implied by --spectrum-x"
		}
		cfg.Profile.Multirail = true
		cfg.Profile.MultirailSet = true
		decisions = append(decisions, DefaultDecision{
			Flag: "--multirail", Value: "true", Reason: reason,
		})
		log.Log.V(1).Info("HW default: --multirail=true", "reason", reason)
	} else {
		log.Log.V(1).Info("HW default: --multirail skipped",
			"current", cfg.Profile.Multirail,
			"configSet", cfg.Profile.MultirailSet,
			"cliSet", opts.MultirailSet)
	}

	// --routing ---------------------------------------------------------
	if cfg.Profile.Routing == "" && opts.Routing == "" {
		cfg.Profile.Routing = config.RoutingDestinationBased
		decisions = append(decisions, DefaultDecision{
			Flag: "--routing", Value: config.RoutingDestinationBased, Reason: "default",
		})
		log.Log.V(1).Info("HW default: --routing=destination-based (default)")
	} else {
		log.Log.V(1).Info("HW default: --routing skipped",
			"current", cfg.Profile.Routing,
			"cliValue", opts.Routing)
	}

	// Spectrum-X-specific defaults --------------------------------------
	// Fire when EITHER the user passed --spectrum-x on the CLI OR the
	// loaded cfg already has spectrumX.enable=true (config-only path).
	// Without one of those signals, multiplane-mode/planes/release
	// defaults are meaningless.
	if opts.SpectrumX || cfgHasSpectrumX {
		applySpectrumXHardwareDefaults(cfg, opts, &decisions)
	}

	return decisions
}

// applySpectrumXHardwareDefaults handles the Spectrum-X-only defaults:
// implicit fabric/deployment/multirail (forced by Spectrum-X),
// --multiplane-mode, --number-of-planes (from GPU platform + east-west PF),
// and --network-operator-release (matched to the chosen RA version).
func applySpectrumXHardwareDefaults(cfg *config.LaunchKitConfig, opts options.Options, decisions *[]DefaultDecision) {
	if cfg.Profile.SpectrumX == nil {
		cfg.Profile.SpectrumX = &config.ProfileSpectrumX{}
	}
	cfg.Profile.SpectrumX.Enable = true
	if cfg.Profile.SpectrumX.IPVersion == "" && opts.IPVersion == "" {
		cfg.Profile.SpectrumX.IPVersion = config.SpectrumXIPVersionIPv4
		*decisions = append(*decisions, DefaultDecision{
			Flag:   "--ip-version",
			Value:  config.SpectrumXIPVersionIPv4,
			Reason: "Spectrum-X default",
		})
	}

	// Implicit defaults: --spectrum-x forces ethernet fabric and sriov
	// deployment. Multirail is handled by ApplyHardwareDefaults so its
	// presence markers are evaluated in one place. Phase 2 cohort validation
	// rejects contradictory user values; here we just fill empty defaults.
	if cfg.Profile.Fabric == "" {
		cfg.Profile.Fabric = "ethernet"
		*decisions = append(*decisions, DefaultDecision{
			Flag: "--fabric", Value: "ethernet", Reason: "implied by --spectrum-x",
		})
	}
	if cfg.Profile.Deployment == "" {
		cfg.Profile.Deployment = "sriov"
		*decisions = append(*decisions, DefaultDecision{
			Flag: "--deployment-type", Value: "sriov", Reason: "implied by --spectrum-x",
		})
	}
	// --multiplane-mode + --number-of-planes. An explicit single-plane value
	// determines its missing companion without consulting hardware. Otherwise,
	// fill the missing values from the platform/NIC pair.
	modeUnset := cfg.Profile.SpectrumX.MultiplaneMode == "" && opts.MultiplaneMode == ""
	planesUnset := cfg.Profile.SpectrumX.NumberOfPlanes == 0 && opts.NumberOfPlanes == 0
	if modeUnset || planesUnset {
		effectiveMode := cfg.Profile.SpectrumX.MultiplaneMode
		if opts.MultiplaneMode != "" {
			effectiveMode = opts.MultiplaneMode
		}
		effectivePlanes := cfg.Profile.SpectrumX.NumberOfPlanes
		if opts.NumberOfPlanes != 0 {
			effectivePlanes = opts.NumberOfPlanes
		}

		mode, planes, ok, reason := "", 0, false, ""
		needsHardwareMode := modeUnset && effectivePlanes != 1
		needsHardwarePlanes := planesUnset && effectiveMode != "none"
		if needsHardwareMode || needsHardwarePlanes {
			mode, planes, ok, reason = spectrumXDefaultsForHardware(cfg.ClusterConfig)
		}

		modeReason := reason
		if modeUnset && effectivePlanes == 1 {
			mode = "none"
			modeReason = "number-of-planes=1 implies single-plane mode"
		}
		planesReason := reason
		if planesUnset && effectiveMode == "none" {
			planes = 1
			planesReason = "multiplane-mode=none implies one plane"
		}

		modeResolved := mode != "" && (ok || effectivePlanes == 1)
		planesResolved := planes != 0 && (ok || effectiveMode == "none")
		log.Log.V(1).Info("HW default: --multiplane-mode / --number-of-planes",
			"groupsConsidered", len(cfg.ClusterConfig),
			"resolvedMode", mode, "resolvedPlanes", planes,
			"modeApplied", modeUnset && modeResolved,
			"planesApplied", planesUnset && planesResolved,
			"hardwareReason", reason)
		if modeUnset && modeResolved {
			cfg.Profile.SpectrumX.MultiplaneMode = mode
			*decisions = append(*decisions, DefaultDecision{
				Flag: "--multiplane-mode", Value: mode,
				Reason: modeReason,
			})
		}
		if planesUnset && planesResolved {
			cfg.Profile.SpectrumX.NumberOfPlanes = planes
			*decisions = append(*decisions, DefaultDecision{
				Flag: "--number-of-planes", Value: fmt.Sprintf("%d", planes),
				Reason: planesReason,
			})
		}
		if (modeUnset && !modeResolved) || (planesUnset && !planesResolved) {
			log.Log.Info("Cannot default --multiplane-mode / --number-of-planes", "reason", reason)
		}
	}

	// --network-operator-release ----------------------------------------
	// SPCXVersion may come from either CLI (opts.SPCXVersion) or the config
	// file. Read the effective value here without applying the CLI option to
	// cfg; ApplyOptionsToConfig remains the single writer for CLI overrides.
	ra := cfg.Profile.SpectrumX.SPCXVersion
	if opts.SPCXVersion != "" {
		ra = opts.SPCXVersion
	}
	currentRelease := ""
	if cfg.NetworkOperator != nil {
		currentRelease = cfg.NetworkOperator.SelectedRelease
	}
	if currentRelease == "" && opts.NetworkOperatorRelease == "" && ra != "" {
		release := config.DefaultSPCXReleaseFor(ra)
		log.Log.V(1).Info("HW default: --network-operator-release",
			"spcxVersion", ra, "resolvedTo", release, "applied", release != "")
		if release != "" {
			if cfg.NetworkOperator == nil {
				cfg.NetworkOperator = &config.NetworkOperatorConfig{}
			}
			cfg.NetworkOperator.SelectedRelease = release
			*decisions = append(*decisions, DefaultDecision{
				Flag:   "--network-operator-release",
				Value:  release,
				Reason: fmt.Sprintf("matches --spectrum-x %s", ra),
			})
		}
	}
}

// dominantLinkType returns the unanimous linkType across all groups,
// normalized to the lowercase form (`ethernet`/`infiniband`) that the
// profile matcher and `--fabric` flag use. ClusterConfig.LinkType
// stores the capitalized sysfs form (`Ethernet`/`InfiniBand`) per
// Unit 5; this normalises to match downstream consumers.
//
// Returns ok=false (with a reason) when any group has empty LinkType
// (probe couldn't confirm a verdict in Unit 5) or groups disagree.
func dominantLinkType(groups []config.ClusterConfig) (linkType string, ok bool, reason string) {
	if len(groups) == 0 {
		return "", false, "no clusterConfig groups"
	}
	var seen string
	for _, g := range groups {
		if g.LinkType == "" {
			return "", false, fmt.Sprintf("group %q has no confirmed linkType (fabric probe couldn't verify)", g.Identifier)
		}
		normalised := strings.ToLower(g.LinkType)
		if seen == "" {
			seen = normalised
			continue
		}
		if seen != normalised {
			return "", false, fmt.Sprintf("groups disagree: %q vs %q", seen, normalised)
		}
	}
	return seen, true, ""
}

// spectrumXDefaultsForHardware returns a single Spectrum-X
// (multiplane-mode, number-of-planes) pair that is valid for every group.
// GPU platform refines the NIC-family fallback because ConnectX-8 can back
// both single-plane H100/H200/B200/GB200 systems and multiplane B300/GB300
// systems. Platform does not distinguish swplb from hwplb: both are available
// on B300 and GB300, so l8k defaults to the GA swplb path and requires an
// explicit override for tech-preview hwplb.
func spectrumXDefaultsForHardware(groups []config.ClusterConfig) (mode string, planes int, ok bool, reason string) {
	if len(groups) == 0 {
		return "", 0, false, "no clusterConfig groups"
	}
	_, hasEastWest, deviceIDErr := config.EastWestDeviceIDForGroups(groups)
	if deviceIDErr != nil {
		return "", 0, false, deviceIDErr.Error()
	}
	if !hasEastWest {
		return "", 0, false, "no east-west PFs"
	}
	var seenPlatform string
	var seenMode string
	var seenPlanes int
	groupsConsidered := 0
	for _, g := range groups {
		deviceID, hasEastWest, idErr := config.EastWestDeviceID(g)
		if !hasEastWest {
			continue
		}
		if idErr != nil {
			return "", 0, false, idErr.Error()
		}
		platform := spectrumXGPUPlatform(g)
		groupMode, groupPlanes, groupReason := spectrumXDefaultForDeviceAndPlatform(deviceID, platform)
		if groupMode == "" {
			return "", 0, false, groupReason
		}
		if groupsConsidered == 0 {
			seenMode = groupMode
			seenPlanes = groupPlanes
			seenPlatform = platform
			reason = groupReason
		} else if seenMode != groupMode || seenPlanes != groupPlanes {
			return "", 0, false, fmt.Sprintf(
				"groups require different Spectrum-X defaults: %s/%d for platform %q vs %s/%d for platform %q",
				seenMode, seenPlanes, seenPlatform, groupMode, groupPlanes, platform)
		} else if seenPlatform != "" && platform != "" && platform != seenPlatform {
			reason = fmt.Sprintf("platforms %s and %s share the %s/%d default", seenPlatform, platform, seenMode, seenPlanes)
		}
		groupsConsidered++
	}
	return seenMode, seenPlanes, true, reason
}

func spectrumXDefaultForDeviceAndPlatform(deviceID, platform string) (mode string, planes int, reason string) {
	switch deviceID {
	case "1021":
		return "none", 1, "ConnectX-7 (deviceID 1021)"
	case "1023":
		switch platform {
		case "H100", "H200", "B200", "GB200":
			return "none", 1, fmt.Sprintf("%s is a single-plane GPU platform (ConnectX-8 deviceID 1023)", platform)
		case "B300":
			return "swplb", 2, "B300 conservative dual-plane SWPLB default; pass 4 explicitly for quad-plane"
		case "GB300":
			return "swplb", 2, "GB300 dual-plane platform; SWPLB is the GA default"
		default:
			return "swplb", 2, "ConnectX-8 (deviceID 1023) fallback; SWPLB is the GA default"
		}
	case "1025":
		return "hwplb", 4, "ConnectX-9 (deviceID 1025)"
	case "a2dc":
		return "none", 1, "BF3 SuperNIC (deviceID a2dc)"
	}
	return "", 0, fmt.Sprintf("east-west PF deviceID %q has no Spectrum-X default", deviceID)
}

func spectrumXGPUPlatform(group config.ClusterConfig) string {
	if platform := spectrumXGPUPlatformToken(group.GPUType); platform != "" {
		return platform
	}
	return spectrumXGPUPlatformToken(group.MachineType)
}

func spectrumXGPUPlatformToken(value string) string {
	tokens := strings.FieldsFunc(strings.ToUpper(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, token := range tokens {
		switch token {
		case "H100", "H200", "B200", "GB200", "B300", "GB300":
			return token
		}
	}
	return ""
}
