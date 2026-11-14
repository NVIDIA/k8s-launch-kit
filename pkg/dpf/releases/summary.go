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
	Version    string             `json:"version" yaml:"version"`
	Operator   OperatorSummary    `json:"operator" yaml:"operator"`
	Components []ComponentSummary `json:"components" yaml:"components"`
	Hardware   []HardwareSummary  `json:"hardware" yaml:"hardware"`
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
	Version string `json:"version" yaml:"version"`
	URL     string `json:"url" yaml:"url"`
	Digest  string `json:"digest,omitempty" yaml:"digest,omitempty"`
}

type HardwareSummary struct {
	Family              string          `json:"family" yaml:"family"`
	Models              []string        `json:"models" yaml:"models"`
	BFB                 ArtifactSummary `json:"bfb" yaml:"bfb"`
	MinimumNICFirmware  string          `json:"minimumNICFirmware,omitempty" yaml:"minimumNICFirmware,omitempty"`
	MinimumHostMFT      string          `json:"minimumHostMFT,omitempty" yaml:"minimumHostMFT,omitempty"`
	MinimumBMCFirmware  string          `json:"minimumBMCFirmware,omitempty" yaml:"minimumBMCFirmware,omitempty"`
	MinimumCECFirmware  string          `json:"minimumCECFirmware,omitempty" yaml:"minimumCECFirmware,omitempty"`
	MinimumATFFirmware  string          `json:"minimumATFFirmware,omitempty" yaml:"minimumATFFirmware,omitempty"`
	MinimumUEFIFirmware string          `json:"minimumUEFIFirmware,omitempty" yaml:"minimumUEFIFirmware,omitempty"`
	MinimumDPUBSP       string          `json:"minimumDPUBSP,omitempty" yaml:"minimumDPUBSP,omitempty"`
	MinimumDPUOS        string          `json:"minimumDPUOS,omitempty" yaml:"minimumDPUOS,omitempty"`
	MinimumGoldenImage  string          `json:"minimumGoldenImage,omitempty" yaml:"minimumGoldenImage,omitempty"`
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
		Version: r.Version,
		Operator: OperatorSummary{Chart: r.Operator.Chart, ChartRepository: r.Operator.ChartRepository,
			Version: r.Operator.Version, ChartDigest: r.Operator.ChartDigest, Image: r.Operator.Image},
		Components: make([]ComponentSummary, 0, len(r.Components)),
		Hardware:   make([]HardwareSummary, 0, len(r.Hardware)),
	}
	for _, c := range r.Components {
		s.Components = append(s.Components, ComponentSummary{Name: c.Name, Version: c.Version, Repository: c.Repository, Applicability: c.Applicability})
	}
	for _, h := range r.Hardware {
		out := HardwareSummary{Family: h.Family, Models: slices.Clone(h.Models),
			BFB: ArtifactSummary{Version: h.BFB.Version, URL: h.BFB.URL, Digest: h.BFB.Digest}}
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
		!summaryDigest(s.Operator.ChartDigest) || !summaryToken(s.Operator.Image) {
		return Release{}, fmt.Errorf("invalid saved DPF release coordinates")
	}
	if len(s.Components) == 0 || len(s.Hardware) == 0 {
		return Release{}, fmt.Errorf("saved DPF software BOM requires components and hardware applicability")
	}
	r := Release{Version: s.Version,
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
		if !summaryToken(h.Family) || len(h.Models) == 0 || !summaryToken(h.BFB.Version) || !summaryHTTPS(h.BFB.URL) ||
			h.BFB.Digest != "" && !summaryDigest(h.BFB.Digest) {
			return Release{}, fmt.Errorf("invalid saved DPF hardware applicability or BFB coordinate")
		}
		for _, model := range h.Models {
			key := strings.ToLower(model)
			if !summaryToken(model) || models[key] {
				return Release{}, fmt.Errorf("invalid or duplicate saved DPF hardware model %q", model)
			}
			models[key] = true
		}
		out := HardwareBOM{Family: h.Family, Models: slices.Clone(h.Models),
			BFB: Artifact{Version: h.BFB.Version, URL: h.BFB.URL, Digest: h.BFB.Digest}, Requirements: []Requirement{}}
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
