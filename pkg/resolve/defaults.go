// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package resolve fills profile-related fields on a loaded
// `LaunchKubernetesConfig` from discovered hardware (defaults) and then
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
// `Options.MultirailSet` to distinguish "not passed" from
// "passed=false".
package resolve

import (
	"fmt"
	"strings"

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
//	--multirail           ← true (always, unless Options.MultirailSet=true).
//	--multiplane-mode     ← per east-west PF deviceID (only when --spectrum-x):
//	                       1021 (CX7) / a2dc (BF3 SuperNIC) → "uniplane"
//	                       1023 (CX8) → "swplb"
//	                       1025 (CX9) → "hwplb"
//	                       Skipped+warned when groups have mixed deviceIDs.
//	--number-of-planes    ← per deviceID (only when --spectrum-x):
//	                       1021 / a2dc → 1, 1023 → 2, 1025 → 4.
//	--network-operator-release ← `config.DefaultSPCXReleaseFor(SPCXVersion)`
//	                            (only when --spectrum-x is set).
//
// `--spectrum-x` itself is NOT defaulted — the user always specifies the
// RA version (per design discussion).
func ApplyHardwareDefaults(cfg *config.LaunchKubernetesConfig, opts options.Options) []DefaultDecision {
	if cfg.Profile == nil {
		cfg.Profile = &config.Profile{}
	}
	var decisions []DefaultDecision

	// --fabric ----------------------------------------------------------
	if cfg.Profile.Fabric == "" && opts.Fabric == "" {
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
	// Bool: skip default only when user explicitly set the flag (either
	// to true or false). MultirailSet captures `cmd.Flag.Changed`.
	if !cfg.Profile.Multirail && !opts.MultirailSet {
		cfg.Profile.Multirail = true
		decisions = append(decisions, DefaultDecision{
			Flag: "--multirail", Value: "true", Reason: "default",
		})
		log.Log.V(1).Info("HW default: --multirail=true (default)")
	} else {
		log.Log.V(1).Info("HW default: --multirail skipped",
			"current", cfg.Profile.Multirail, "userSet", opts.MultirailSet)
	}

	// Spectrum-X-specific defaults --------------------------------------
	// Fire when EITHER the user passed --spectrum-x on the CLI OR the
	// loaded cfg already has spectrumX.enable=true (config-only path).
	// Without one of those signals, multiplane-mode/planes/release
	// defaults are meaningless.
	cfgHasSpectrumX := cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable
	if opts.SpectrumX || cfgHasSpectrumX {
		applySpectrumXHardwareDefaults(cfg, opts, &decisions)
	}

	return decisions
}

// applySpectrumXHardwareDefaults handles the Spectrum-X-only defaults:
// implicit fabric/deployment/multirail (forced by Spectrum-X),
// --multiplane-mode, --number-of-planes (from east-west PF deviceID),
// and --network-operator-release (matched to the chosen RA version).
func applySpectrumXHardwareDefaults(cfg *config.LaunchKubernetesConfig, opts options.Options, decisions *[]DefaultDecision) {
	if cfg.Profile.SpectrumX == nil {
		cfg.Profile.SpectrumX = &config.ProfileSpectrumX{}
	}
	cfg.Profile.SpectrumX.Enable = true
	if cfg.Profile.SpectrumX.SPCXVersion == "" && opts.SPCXVersion != "" {
		cfg.Profile.SpectrumX.SPCXVersion = opts.SPCXVersion
	}

	// Implicit defaults: --spectrum-x forces ethernet fabric, sriov
	// deployment, and multirail. Phase 2 cohort validation rejects
	// contradictory user values; here we just fill the empty defaults.
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
	if !cfg.Profile.Multirail && !opts.MultirailSet {
		cfg.Profile.Multirail = true
		*decisions = append(*decisions, DefaultDecision{
			Flag: "--multirail", Value: "true", Reason: "implied by --spectrum-x",
		})
	}

	// --multiplane-mode + --number-of-planes (paired — both come from
	// the same deviceID).
	modeUnset := cfg.Profile.SpectrumX.MultiplaneMode == "" && opts.MultiplaneMode == ""
	planesUnset := cfg.Profile.SpectrumX.NumberOfPlanes == 0 && opts.NumberOfPlanes == 0
	if modeUnset || planesUnset {
		mode, planes, ok, reason := spectrumXDefaultsForDeviceID(cfg.ClusterConfig)
		log.Log.V(1).Info("HW default: --multiplane-mode / --number-of-planes",
			"groupsConsidered", len(cfg.ClusterConfig),
			"resolvedMode", mode, "resolvedPlanes", planes,
			"applied", ok, "reason", reason)
		if ok {
			if modeUnset {
				cfg.Profile.SpectrumX.MultiplaneMode = mode
				*decisions = append(*decisions, DefaultDecision{
					Flag: "--multiplane-mode", Value: mode,
					Reason: reason,
				})
			}
			if planesUnset {
				cfg.Profile.SpectrumX.NumberOfPlanes = planes
				*decisions = append(*decisions, DefaultDecision{
					Flag: "--number-of-planes", Value: fmt.Sprintf("%d", planes),
					Reason: reason,
				})
			}
		} else {
			log.Log.Info("Cannot default --multiplane-mode / --number-of-planes", "reason", reason)
		}
	}

	// --network-operator-release ----------------------------------------
	// SPCXVersion may come from either CLI (opts.SPCXVersion) or
	// config-file (cfg.Profile.SpectrumX.SPCXVersion). Prefer the
	// resolved cfg value since CLI may not have been set in the
	// config-only Spectrum-X path.
	ra := cfg.Profile.SpectrumX.SPCXVersion
	if ra == "" {
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

// spectrumXDefaultsForDeviceID returns the Spectrum-X (multiplane-mode,
// number-of-planes) pair that matches the deviceID of the east-west
// PFs across all groups. ok=false when groups have mixed deviceIDs or
// a deviceID isn't in the registered Spectrum-X mapping.
func spectrumXDefaultsForDeviceID(groups []config.ClusterConfig) (mode string, planes int, ok bool, reason string) {
	if len(groups) == 0 {
		return "", 0, false, "no clusterConfig groups"
	}
	var seenID string
	for _, g := range groups {
		for _, pf := range g.PFs {
			if pf.Traffic != "east-west" {
				continue
			}
			normID := strings.ToLower(pf.DeviceID)
			if seenID == "" {
				seenID = normID
				continue
			}
			if seenID != normID {
				return "", 0, false, fmt.Sprintf("east-west PFs have mixed deviceIDs: %q vs %q", seenID, normID)
			}
		}
	}
	if seenID == "" {
		return "", 0, false, "no east-west PFs"
	}
	switch seenID {
	case "1021":
		return "uniplane", 1, true, "ConnectX-7 (deviceID 1021)"
	case "1023":
		return "swplb", 2, true, "ConnectX-8 (deviceID 1023)"
	case "1025":
		return "hwplb", 4, true, "ConnectX-9 (deviceID 1025)"
	case "a2dc":
		return "uniplane", 1, true, "BF3 SuperNIC (deviceID a2dc)"
	}
	return "", 0, false, fmt.Sprintf("east-west PF deviceID %q has no Spectrum-X default", seenID)
}
