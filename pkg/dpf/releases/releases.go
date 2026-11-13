// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package releases owns the embedded, release-specific DPF software bill of
// materials. Catalog entries describe software coordinates and documented
// requirements; they do not qualify hardware or authorize installation.
package releases

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultRelease is deliberately pinned. Adding a catalog entry never selects it
// automatically; promoting the default is a separate reviewed change.
const DefaultRelease = "26.4.0"

//go:embed releases.yaml
var catalogYAML []byte

// Source identifies the authoritative upstream material used to curate a BOM.
// Revision is a Git commit or the digest of a downloaded release artifact.
type Source struct {
	ID       string `json:"id" yaml:"id"`
	URL      string `json:"url" yaml:"url"`
	Revision string `json:"revision" yaml:"revision"`
}

type Operator struct {
	Chart           string `json:"chart" yaml:"chart"`
	ChartRepository string `json:"chartRepository" yaml:"chartRepository"`
	Version         string `json:"version" yaml:"version"`
	ChartDigest     string `json:"chartDigest" yaml:"chartDigest"`
	Image           string `json:"image" yaml:"image"`
	SourceRef       string `json:"sourceRef" yaml:"sourceRef"`
}

// Component is a release coordinate, not evidence that the component is present.
// Applicability distinguishes base components from conditional and optional ones.
type Component struct {
	Name          string `json:"name" yaml:"name"`
	Version       string `json:"version" yaml:"version"`
	Repository    string `json:"repository" yaml:"repository"`
	Applicability string `json:"applicability" yaml:"applicability"`
	SourceRef     string `json:"sourceRef" yaml:"sourceRef"`
}

type Artifact struct {
	Version    string `json:"version" yaml:"version"`
	URL        string `json:"url" yaml:"url"`
	Digest     string `json:"digest,omitempty" yaml:"digest,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message    string `json:"message,omitempty" yaml:"message,omitempty"`
	SourceRef  string `json:"sourceRef" yaml:"sourceRef"`
}

// VersionRange records a release's management-cluster Kubernetes support
// range. It is independent from DPU hardware requirements.
type VersionRange struct {
	Minimum string `json:"minimum" yaml:"minimum"`
	Maximum string `json:"maximum" yaml:"maximum"`
}

// BlueFieldSoftware contains BF4 provisioning artifacts. BF4 uses an OS ISO
// and PSID-specific PLDM bundles rather than a BF3 BFB.
type BlueFieldSoftware struct {
	OSISO        Artifact            `json:"osIso" yaml:"osIso"`
	PLDMFwBundle map[string]Artifact `json:"pldmFwBundle,omitempty" yaml:"pldmFwBundle,omitempty"`
}

// Requirement distinguishes a minimum capability version from a desired target.
// Unknown requirements have no numeric constraint and retain a reason. Scope
// identifies where a requirement applies, not where it must be measured.
type Requirement struct {
	Component  string `json:"component" yaml:"component"`
	Scope      string `json:"scope" yaml:"scope"`
	State      string `json:"state" yaml:"state"`
	Minimum    string `json:"minimum,omitempty" yaml:"minimum,omitempty"`
	Target     string `json:"target,omitempty" yaml:"target,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message    string `json:"message,omitempty" yaml:"message,omitempty"`
	SourceRef  string `json:"sourceRef" yaml:"sourceRef"`
}

type HardwareBOM struct {
	Family            string             `json:"family" yaml:"family"`
	Models            []string           `json:"models" yaml:"models"`
	SupportLevel      string             `json:"supportLevel,omitempty" yaml:"supportLevel,omitempty"`
	BFB               *Artifact          `json:"bfb,omitempty" yaml:"bfb,omitempty"`
	BlueFieldSoftware *BlueFieldSoftware `json:"blueFieldSoftware,omitempty" yaml:"blueFieldSoftware,omitempty"`
	Requirements      []Requirement      `json:"requirements" yaml:"requirements"`
}

type Release struct {
	Version             string        `json:"version" yaml:"version"`
	SupportedKubernetes *VersionRange `json:"supportedKubernetes,omitempty" yaml:"supportedKubernetes,omitempty"`
	Operator            Operator      `json:"operator" yaml:"operator"`
	Components          []Component   `json:"components" yaml:"components"`
	Hardware            []HardwareBOM `json:"hardware" yaml:"hardware"`
	Sources             []Source      `json:"sources" yaml:"sources"`
	Limitations         []string      `json:"limitations" yaml:"limitations"`
}

type catalog struct {
	Releases map[string]Release `yaml:"releases"`
}

// Resolve returns a fresh BOM with no shared mutable values. It performs no
// network access. Empty means the pinned default; callers with presence-aware
// configuration must reject a supplied empty value before calling Resolve.
func Resolve(version string) (Release, error) {
	if version == "" {
		version = DefaultRelease
	}
	c, err := load(catalogYAML)
	if err != nil {
		return Release{}, err
	}
	r, ok := c.Releases[version]
	if !ok {
		return Release{}, fmt.Errorf("unknown DPF release %q; catalog releases: %s", version, strings.Join(keys(c), ", "))
	}
	return r, nil
}

// SupportedReleases returns catalog keys. Presence does not assert runtime
// qualification for every component, hardware model or deployment profile.
func SupportedReleases() []string {
	c, err := load(catalogYAML)
	if err != nil {
		return nil
	}
	return keys(c)
}

func keys(c catalog) []string {
	out := make([]string, 0, len(c.Releases))
	for k := range c.Releases {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// RequirementsFor returns a copy of the hardware BOM for an exact documented
// model. Generic family names, unknown models and nil receivers do not match;
// callers must retain an explicit applicability-unresolved result.
func (r *Release) RequirementsFor(model string) (HardwareBOM, bool) {
	if r == nil {
		return HardwareBOM{}, false
	}
	model = strings.TrimSpace(model)
	for _, h := range r.Hardware {
		for _, supported := range h.Models {
			if strings.EqualFold(model, supported) {
				h.Models = slices.Clone(h.Models)
				h.Requirements = slices.Clone(h.Requirements)
				return h, true
			}
		}
	}
	return HardwareBOM{}, false
}

func load(data []byte) (catalog, error) {
	var c catalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return catalog{}, fmt.Errorf("invalid embedded DPF release catalog: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return catalog{}, fmt.Errorf("DPF release catalog must contain one document")
	}
	if len(c.Releases) == 0 {
		return catalog{}, fmt.Errorf("DPF release catalog has no entries")
	}
	for key, r := range c.Releases {
		if err := validate(key, r); err != nil {
			return catalog{}, fmt.Errorf("invalid DPF release %q: %w", key, err)
		}
	}
	return c, nil
}

func validate(key string, r Release) error {
	if r.Version != key || r.Operator.Version != "v"+key || r.Operator.Chart == "" || r.Operator.ChartRepository == "" || r.Operator.Image == "" || r.Operator.ChartDigest == "" {
		return fmt.Errorf("missing or inconsistent release coordinates")
	}
	sources := map[string]bool{}
	for _, s := range r.Sources {
		if s.ID == "" || sources[s.ID] || !strings.HasPrefix(s.URL, "https://") || s.Revision == "" {
			return fmt.Errorf("missing or duplicate authoritative source")
		}
		sources[s.ID] = true
	}
	if !sources[r.Operator.SourceRef] {
		return fmt.Errorf("operator source is not declared")
	}
	if r.SupportedKubernetes != nil && !validVersionRange(*r.SupportedKubernetes) {
		return fmt.Errorf("invalid supported Kubernetes version range")
	}
	components := map[string]bool{}
	for _, c := range r.Components {
		if c.Name == "" || components[c.Name] || c.Version == "" || c.Repository == "" || !sources[c.SourceRef] || !slices.Contains([]string{"base", "conditional", "optional"}, c.Applicability) {
			return fmt.Errorf("invalid component coordinate or source")
		}
		components[c.Name] = true
	}
	models := map[string]bool{}
	for _, h := range r.Hardware {
		bf4FamilyOnly := h.Family == "BlueField-4" && len(h.Models) == 0 && h.SupportLevel == "beta" && h.BlueFieldSoftware != nil && h.BFB == nil
		if h.Family == "" || (len(h.Models) == 0 && !bf4FamilyOnly) || h.SupportLevel != "" && h.SupportLevel != "ga" && h.SupportLevel != "beta" {
			return fmt.Errorf("invalid hardware applicability or artifact provenance")
		}
		if bf4FamilyOnly {
			if err := validateBlueFieldSoftware(*h.BlueFieldSoftware, sources); err != nil {
				return err
			}
		} else if h.BlueFieldSoftware != nil || h.BFB == nil || h.BFB.Version == "" || !strings.HasPrefix(h.BFB.URL, "https://") || !sources[h.BFB.SourceRef] || (h.BFB.Digest == "" && (h.BFB.ReasonCode == "" || h.BFB.Message == "")) {
			return fmt.Errorf("invalid hardware applicability or artifact provenance")
		}
		for _, model := range h.Models {
			model = strings.ToLower(strings.TrimSpace(model))
			if model == "" || models[model] {
				return fmt.Errorf("missing or ambiguous hardware model")
			}
			models[model] = true
		}
		seen := map[string]bool{}
		for _, req := range h.Requirements {
			if req.Component == "" || req.Scope == "" || seen[req.Component+"/"+req.Scope] || !sources[req.SourceRef] {
				return fmt.Errorf("invalid hardware requirement or source")
			}
			seen[req.Component+"/"+req.Scope] = true
			switch req.State {
			case "known":
				if req.Minimum == "" && req.Target == "" {
					return fmt.Errorf("known requirement has no minimum or target")
				}
			case "unknown":
				if req.Minimum != "" || req.Target != "" || req.ReasonCode == "" || req.Message == "" {
					return fmt.Errorf("unknown requirement needs a reason and no inferred version")
				}
			default:
				return fmt.Errorf("invalid requirement state")
			}
		}
	}
	return nil
}

func validVersionRange(r VersionRange) bool {
	parseMinor := func(version string) (int, bool) {
		parts := strings.Split(version, ".")
		if len(parts) != 2 {
			return 0, false
		}
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		if majorErr != nil || minorErr != nil || major < 1 || minor < 0 {
			return 0, false
		}
		return major*1000 + minor, true
	}
	minimum, minOK := parseMinor(r.Minimum)
	maximum, maxOK := parseMinor(r.Maximum)
	return minOK && maxOK && maximum >= minimum
}

func validateBlueFieldSoftware(software BlueFieldSoftware, sources map[string]bool) error {
	if !validArtifact(software.OSISO, sources) {
		return fmt.Errorf("invalid BlueFieldSoftware OS ISO coordinate")
	}
	for psid, artifact := range software.PLDMFwBundle {
		if psid == "" || !validArtifact(artifact, sources) {
			return fmt.Errorf("invalid BlueFieldSoftware PLDM bundle coordinate")
		}
	}
	return nil
}

func validArtifact(a Artifact, sources map[string]bool) bool {
	if !sources[a.SourceRef] {
		return false
	}
	if a.Version == "" && a.URL == "" && a.Digest == "" {
		return a.ReasonCode != "" && a.Message != ""
	}
	return a.Version != "" && strings.HasPrefix(a.URL, "https://") && (a.Digest != "" || a.ReasonCode != "" && a.Message != "")
}
