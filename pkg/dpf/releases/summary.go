// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Summary is the persisted software BOM. Catalog research provenance remains in
// Release; omitted minima mean unknown requirements, never inferred constraints.
type Summary struct {
	Version             string             `json:"version" yaml:"version"`
	SupportedKubernetes *VersionRange      `json:"supportedKubernetes,omitempty" yaml:"supportedKubernetes,omitempty"`
	Operator            OperatorSummary    `json:"operator" yaml:"operator"`
	Components          []ComponentSummary `json:"components" yaml:"components"`
	Hardware            []HardwareSummary  `json:"hardware" yaml:"hardware"`
}

type OperatorSummary struct {
	Chart           string `json:"chart" yaml:"chart"`
	ChartRepository string `json:"chartRepository" yaml:"chartRepository"`
	Version         string `json:"version" yaml:"version"`
	ChartDigest     string `json:"chartDigest" yaml:"chartDigest"`
	Image           string `json:"image" yaml:"image"`
}

type ComponentSummary struct {
	Name          string `json:"name" yaml:"name"`
	Version       string `json:"version" yaml:"version"`
	Repository    string `json:"repository" yaml:"repository"`
	Applicability string `json:"applicability" yaml:"applicability"`
}

type ArtifactSummary struct {
	Version    string `json:"version,omitempty" yaml:"version,omitempty"`
	URL        string `json:"url,omitempty" yaml:"url,omitempty"`
	Digest     string `json:"digest,omitempty" yaml:"digest,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message    string `json:"message,omitempty" yaml:"message,omitempty"`
}

type BlueFieldSoftwareSummary struct {
	OSISO        ArtifactSummary            `json:"osIso" yaml:"osIso"`
	PLDMFwBundle map[string]ArtifactSummary `json:"pldmFwBundle,omitempty" yaml:"pldmFwBundle,omitempty"`
}

type HardwareSummary struct {
	Family              string                    `json:"family" yaml:"family"`
	Models              []string                  `json:"models" yaml:"models"`
	SupportLevel        string                    `json:"supportLevel,omitempty" yaml:"supportLevel,omitempty"`
	BFB                 *ArtifactSummary          `json:"bfb,omitempty" yaml:"bfb,omitempty"`
	BlueFieldSoftware   *BlueFieldSoftwareSummary `json:"blueFieldSoftware,omitempty" yaml:"blueFieldSoftware,omitempty"`
	MinimumNICFirmware  string                    `json:"minimumNICFirmware,omitempty" yaml:"minimumNICFirmware,omitempty"`
	MinimumHostMFT      string                    `json:"minimumHostMFT,omitempty" yaml:"minimumHostMFT,omitempty"`
	MinimumBMCFirmware  string                    `json:"minimumBMCFirmware,omitempty" yaml:"minimumBMCFirmware,omitempty"`
	MinimumCECFirmware  string                    `json:"minimumCECFirmware,omitempty" yaml:"minimumCECFirmware,omitempty"`
	MinimumATFFirmware  string                    `json:"minimumATFFirmware,omitempty" yaml:"minimumATFFirmware,omitempty"`
	MinimumUEFIFirmware string                    `json:"minimumUEFIFirmware,omitempty" yaml:"minimumUEFIFirmware,omitempty"`
	MinimumDPUBSP       string                    `json:"minimumDPUBSP,omitempty" yaml:"minimumDPUBSP,omitempty"`
	MinimumDPUOS        string                    `json:"minimumDPUOS,omitempty" yaml:"minimumDPUOS,omitempty"`
	MinimumGoldenImage  string                    `json:"minimumGoldenImage,omitempty" yaml:"minimumGoldenImage,omitempty"`
}

// minimumFields fixes the supported requirement names and measurement scopes.
// It does not consult a newer catalog when decoding a saved inventory.
func (h *HardwareSummary) minimumFields() []struct {
	component, scope string
	value            *string
} {
	return []struct {
		component, scope string
		value            *string
	}{
		{"nic", "dpu", &h.MinimumNICFirmware},
		{"mft", "host", &h.MinimumHostMFT},
		{"bmc", "dpu", &h.MinimumBMCFirmware},
		{"cec", "dpu", &h.MinimumCECFirmware},
		{"atf", "dpu", &h.MinimumATFFirmware},
		{"uefi", "dpu", &h.MinimumUEFIFirmware},
		{"dpu-bsp", "dpu", &h.MinimumDPUBSP},
		{"dpu-os", "dpu", &h.MinimumDPUOS},
		{"golden", "dpu", &h.MinimumGoldenImage},
	}
}

// Summarize projects a curated release into facts needed by inventory consumers.
// It returns independent slices and never adds a digest or a numeric minimum.
func Summarize(r Release) Summary {
	s := Summary{
		Version:             r.Version,
		SupportedKubernetes: cloneVersionRange(r.SupportedKubernetes),
		Operator: OperatorSummary{Chart: r.Operator.Chart, ChartRepository: r.Operator.ChartRepository,
			Version: r.Operator.Version, ChartDigest: r.Operator.ChartDigest, Image: r.Operator.Image},
		Components: make([]ComponentSummary, 0, len(r.Components)),
		Hardware:   make([]HardwareSummary, 0, len(r.Hardware)),
	}
	for _, c := range r.Components {
		s.Components = append(s.Components, ComponentSummary{Name: c.Name, Version: c.Version, Repository: c.Repository, Applicability: c.Applicability})
	}
	for _, h := range r.Hardware {
		out := HardwareSummary{Family: h.Family, Models: slices.Clone(h.Models), SupportLevel: h.SupportLevel}
		if h.BFB != nil {
			out.BFB = &ArtifactSummary{Version: h.BFB.Version, URL: h.BFB.URL, Digest: h.BFB.Digest}
		}
		if h.BlueFieldSoftware != nil {
			software := &BlueFieldSoftwareSummary{OSISO: artifactSummary(h.BlueFieldSoftware.OSISO)}
			if len(h.BlueFieldSoftware.PLDMFwBundle) > 0 {
				software.PLDMFwBundle = make(map[string]ArtifactSummary, len(h.BlueFieldSoftware.PLDMFwBundle))
				for psid, artifact := range h.BlueFieldSoftware.PLDMFwBundle {
					software.PLDMFwBundle[psid] = artifactSummary(artifact)
				}
			}
			out.BlueFieldSoftware = software
		}
		for _, field := range out.minimumFields() {
			for _, requirement := range h.Requirements {
				if requirement.Component == field.component && requirement.Scope == field.scope && requirement.State == "known" {
					*field.value = requirement.Minimum
				}
			}
		}
		s.Hardware = append(s.Hardware, out)
	}
	return s
}

var (
	summaryReleaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	summaryMinimumVersion = regexp.MustCompile(`^[0-9]+([.-][0-9]+)*$`)
)

// Release restores consumer semantics from saved facts, without resolving the
// current catalog. Unknown minima stay unknown; BFB targets are never minima.
// The caller must use strict YAML/JSON decoding to reject unknown fields.
func (s Summary) Release() (Release, error) {
	if !summaryReleaseVersion.MatchString(s.Version) || s.Operator.Version != "v"+s.Version ||
		!summaryToken(s.Operator.Chart) || !summaryHTTPS(s.Operator.ChartRepository) ||
		!summaryDigest(s.Operator.ChartDigest) || !summaryToken(s.Operator.Image) ||
		s.SupportedKubernetes != nil && !validVersionRange(*s.SupportedKubernetes) {
		return Release{}, fmt.Errorf("invalid saved DPF release coordinates")
	}
	if len(s.Components) == 0 || len(s.Hardware) == 0 {
		return Release{}, fmt.Errorf("saved DPF software BOM requires components and hardware applicability")
	}
	r := Release{Version: s.Version, SupportedKubernetes: cloneVersionRange(s.SupportedKubernetes),
		Operator: Operator{Chart: s.Operator.Chart, ChartRepository: s.Operator.ChartRepository,
			Version: s.Operator.Version, ChartDigest: s.Operator.ChartDigest, Image: s.Operator.Image},
		Components: make([]Component, 0, len(s.Components)), Hardware: make([]HardwareBOM, 0, len(s.Hardware))}
	components := map[string]bool{}
	for _, c := range s.Components {
		if !summaryToken(c.Name) || components[c.Name] || !summaryToken(c.Version) || !summaryToken(c.Repository) ||
			!slices.Contains([]string{"base", "conditional", "optional"}, c.Applicability) {
			return Release{}, fmt.Errorf("invalid or duplicate saved DPF component %q", c.Name)
		}
		components[c.Name] = true
		r.Components = append(r.Components, Component{Name: c.Name, Version: c.Version, Repository: c.Repository, Applicability: c.Applicability})
	}
	models := map[string]bool{}
	for _, h := range s.Hardware {
		bf4FamilyOnly := h.Family == "BlueField-4" && len(h.Models) == 0 && h.SupportLevel == "beta" && h.BlueFieldSoftware != nil && h.BFB == nil
		if !summaryToken(h.Family) || len(h.Models) == 0 && !bf4FamilyOnly || h.SupportLevel != "" && h.SupportLevel != "ga" && h.SupportLevel != "beta" {
			return Release{}, fmt.Errorf("invalid saved DPF hardware applicability or BFB coordinate")
		}
		if bf4FamilyOnly {
			software, err := blueFieldSoftwareFromSummary(*h.BlueFieldSoftware)
			if err != nil {
				return Release{}, err
			}
			out := HardwareBOM{Family: h.Family, SupportLevel: h.SupportLevel, BlueFieldSoftware: software, Requirements: []Requirement{}}
			r.Hardware = append(r.Hardware, out)
			continue
		}
		if h.BlueFieldSoftware != nil || h.BFB == nil || !summaryToken(h.BFB.Version) || !summaryHTTPS(h.BFB.URL) || h.BFB.Digest != "" && !summaryDigest(h.BFB.Digest) {
			return Release{}, fmt.Errorf("invalid saved DPF hardware applicability or BFB coordinate")
		}
		for _, model := range h.Models {
			key := strings.ToLower(model)
			if !summaryToken(model) || models[key] {
				return Release{}, fmt.Errorf("invalid or duplicate saved DPF hardware model %q", model)
			}
			models[key] = true
		}
		bfb := &Artifact{Version: h.BFB.Version, URL: h.BFB.URL, Digest: h.BFB.Digest}
		out := HardwareBOM{Family: h.Family, Models: slices.Clone(h.Models), SupportLevel: h.SupportLevel,
			BFB: bfb, Requirements: []Requirement{}}
		for _, field := range h.minimumFields() {
			requirement := Requirement{Component: field.component, Scope: field.scope, State: "unknown",
				ReasonCode: "RELEASE_REQUIREMENT_NOT_DOCUMENTED", Message: "The saved release BOM records no minimum for this component."}
			if *field.value != "" {
				if !summaryMinimumVersion.MatchString(*field.value) {
					return Release{}, fmt.Errorf("invalid saved DPF %s minimum", field.component)
				}
				requirement.State, requirement.Minimum = "known", *field.value
				requirement.ReasonCode, requirement.Message = "", ""
			}
			out.Requirements = append(out.Requirements, requirement)
		}
		r.Hardware = append(r.Hardware, out)
	}
	return r, nil
}

func artifactSummary(a Artifact) ArtifactSummary {
	return ArtifactSummary{Version: a.Version, URL: a.URL, Digest: a.Digest, ReasonCode: a.ReasonCode, Message: a.Message}
}

func blueFieldSoftwareFromSummary(s BlueFieldSoftwareSummary) (*BlueFieldSoftware, error) {
	osISO := Artifact{Version: s.OSISO.Version, URL: s.OSISO.URL, Digest: s.OSISO.Digest,
		ReasonCode: s.OSISO.ReasonCode, Message: s.OSISO.Message}
	if !validArtifactSummary(s.OSISO) {
		return nil, fmt.Errorf("invalid saved BlueFieldSoftware OS ISO coordinate")
	}
	software := &BlueFieldSoftware{OSISO: osISO}
	if len(s.PLDMFwBundle) > 0 {
		software.PLDMFwBundle = make(map[string]Artifact, len(s.PLDMFwBundle))
		for psid, item := range s.PLDMFwBundle {
			if !summaryToken(psid) || !validArtifactSummary(item) {
				return nil, fmt.Errorf("invalid saved BlueFieldSoftware PLDM coordinate for PSID %q", psid)
			}
			software.PLDMFwBundle[psid] = Artifact{Version: item.Version, URL: item.URL, Digest: item.Digest,
				ReasonCode: item.ReasonCode, Message: item.Message}
		}
	}
	return software, nil
}

func validArtifactSummary(a ArtifactSummary) bool {
	if a.Version == "" && a.URL == "" && a.Digest == "" {
		return a.ReasonCode != "" && a.Message != ""
	}
	return summaryToken(a.Version) && summaryHTTPS(a.URL) && (a.Digest == "" && a.ReasonCode != "" && a.Message != "" || a.Digest != "" && summaryDigest(a.Digest))
}

func cloneVersionRange(r *VersionRange) *VersionRange {
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}

func summaryToken(s string) bool {
	return s != "" && strings.IndexFunc(s, unicode.IsSpace) < 0 && strings.IndexFunc(s, unicode.IsControl) < 0
}

func summaryHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && summaryToken(s) && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func summaryDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || s != strings.ToLower(s) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return err == nil && len(decoded) == 32
}
