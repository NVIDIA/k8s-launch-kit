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
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
)

// DefaultRelease is the generic-inventory default. HBN-BGP uses the separately
// reviewed default declared under defaultUseCases in the embedded catalog.
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

// HBNWorkload records release-owned artifact coordinates used by the native
// HBN-BGP renderer. It is a default tuple; an explicit site configuration may
// select a custom tuple, which is recorded and verified independently.
type HBNWorkload struct {
	ChartRepository string               `json:"chartRepository" yaml:"chartRepository"`
	Chart           string               `json:"chart" yaml:"chart"`
	ChartVersion    string               `json:"chartVersion" yaml:"chartVersion"`
	ChartDigest     string               `json:"chartDigest" yaml:"chartDigest"`
	ImageRepository string               `json:"imageRepository" yaml:"imageRepository"`
	ImageTag        string               `json:"imageTag" yaml:"imageTag"`
	ImageDigest     string               `json:"imageDigest" yaml:"imageDigest"`
	StartupVersion  string               `json:"startupVersion" yaml:"startupVersion"`
	SourceRef       string               `json:"sourceRef" yaml:"sourceRef"`
	ResetSafety     *ResetSafetyContract `json:"resetSafety,omitempty" yaml:"resetSafety,omitempty"`
}

// ResetSafetyContract is the exact firmware/model contract consumed by the
// native Redfish reset-safety reader. It does not qualify broader BF4 support.
type ResetSafetyContract struct {
	Contract    string   `json:"contract" yaml:"contract"`
	BMCFirmware string   `json:"bmcFirmware" yaml:"bmcFirmware"`
	CPUFirmware string   `json:"cpuFirmware" yaml:"cpuFirmware"`
	Models      []string `json:"models" yaml:"models"`
}

// Capabilities identifies reviewed implementation contracts for a release.
// These flags do not qualify a hardware model or prove runtime readiness.
type Capabilities struct {
	NativeOperatorProjection bool `json:"nativeOperatorProjection,omitempty" yaml:"nativeOperatorProjection,omitempty"`
	BF4ProvisioningAPI       bool `json:"bf4ProvisioningAPI,omitempty" yaml:"bf4ProvisioningAPI,omitempty"`
}

type Release struct {
	Version             string        `json:"version" yaml:"version"`
	SupportedKubernetes *VersionRange `json:"supportedKubernetes,omitempty" yaml:"supportedKubernetes,omitempty"`
	// TenantKubernetesVersion is the release-owned Kamaji tenant control-plane
	// version. It is separate from SupportedKubernetes, which describes the
	// management cluster running the DPF operator.
	TenantKubernetesVersion string        `json:"tenantKubernetesVersion,omitempty" yaml:"tenantKubernetesVersion,omitempty"`
	Operator                Operator      `json:"operator" yaml:"operator"`
	UseCases                []string      `json:"useCases,omitempty" yaml:"useCases,omitempty"`
	HBNWorkload             *HBNWorkload  `json:"hbnWorkload,omitempty" yaml:"hbnWorkload,omitempty"`
	Capabilities            Capabilities  `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Components              []Component   `json:"components" yaml:"components"`
	Hardware                []HardwareBOM `json:"hardware" yaml:"hardware"`
	Sources                 []Source      `json:"sources" yaml:"sources"`
	Limitations             []string      `json:"limitations" yaml:"limitations"`
}

type catalog struct {
	DefaultUseCases map[string]string  `yaml:"defaultUseCases"`
	Releases        map[string]Release `yaml:"releases"`
}

const UseCaseHBNBGP = "hbn-bgp"

var (
	hbnVersionPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	hbnTagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	hbnImageRepoPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	hbnStartupPattern   = regexp.MustCompile(`^HBN [0-9]+\.[0-9]+\.[0-9]+$`)
)

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

// DefaultForUseCase resolves the reviewed default release for a use case.
// It is deliberately separate from DefaultRelease, which serves generic
// inventory without a selected workflow.
func DefaultForUseCase(useCase string) (Release, error) {
	c, err := load(catalogYAML)
	if err != nil {
		return Release{}, err
	}
	version, ok := c.DefaultUseCases[useCase]
	if !ok {
		return Release{}, fmt.Errorf("no default DPF release is declared for use case %q", useCase)
	}
	r, ok := c.Releases[version]
	if !ok || !r.SupportsUseCase(useCase) {
		return Release{}, fmt.Errorf("default DPF release %q does not support use case %q", version, useCase)
	}
	return r, nil
}

// SupportsUseCase reports only a catalog-declared release contract.
func (r Release) SupportsUseCase(useCase string) bool {
	return slices.Contains(r.UseCases, useCase)
}

// SupportedUseCaseReleases returns catalog rows with an explicit use-case
// contract. Catalog membership alone does not imply workflow support.
func SupportedUseCaseReleases(useCase string) []string {
	c, err := load(catalogYAML)
	if err != nil {
		return nil
	}
	versions := make([]string, 0, len(c.Releases))
	for version, release := range c.Releases {
		if release.SupportsUseCase(useCase) {
			versions = append(versions, version)
		}
	}
	slices.Sort(versions)
	return versions
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
	for useCase, version := range c.DefaultUseCases {
		r, ok := c.Releases[version]
		if useCase == "" || !ok || !r.SupportsUseCase(useCase) {
			return catalog{}, fmt.Errorf("default release for use case %q is missing or unsupported", useCase)
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
	seenUseCases := map[string]bool{}
	for _, useCase := range r.UseCases {
		if useCase != UseCaseHBNBGP || seenUseCases[useCase] {
			return fmt.Errorf("invalid or duplicate supported use case %q", useCase)
		}
		seenUseCases[useCase] = true
	}
	if seenUseCases[UseCaseHBNBGP] {
		if r.HBNWorkload == nil || !r.Capabilities.NativeOperatorProjection {
			return fmt.Errorf("HBN-BGP support requires workload coordinates and the native operator projection contract")
		}
		if err := validateHBNWorkload(*r.HBNWorkload, sources); err != nil {
			return err
		}
	} else if r.HBNWorkload != nil || r.Capabilities.NativeOperatorProjection || r.Capabilities.BF4ProvisioningAPI {
		return fmt.Errorf("HBN workload coordinates and native HBN capabilities require HBN-BGP support")
	}
	if r.SupportedKubernetes != nil && !validVersionRange(*r.SupportedKubernetes) {
		return fmt.Errorf("invalid supported Kubernetes version range")
	}
	if r.TenantKubernetesVersion != "" && !validTenantKubernetesVersion(r.TenantKubernetesVersion) {
		return fmt.Errorf("invalid tenant Kubernetes version")
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

func validateHBNWorkload(w HBNWorkload, sources map[string]bool) error {
	if !validHBNWorkloadCoordinates(w) || !sources[w.SourceRef] {
		return fmt.Errorf("invalid HBN workload coordinates or source")
	}
	if !validResetSafetyContract(w.ResetSafety) {
		return fmt.Errorf("invalid HBN reset-safety contract")
	}
	return nil
}

func validResetSafetyContract(r *ResetSafetyContract) bool {
	if r == nil {
		return true
	}
	if r.Contract != "bf4-reset-safety-v1" || !summaryToken(r.BMCFirmware) || !summaryToken(r.CPUFirmware) || len(r.Models) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, model := range r.Models {
		if !summaryToken(model) || seen[model] {
			return false
		}
		seen[model] = true
	}
	return true
}

func validHBNWorkloadCoordinates(w HBNWorkload) bool {
	return summaryHTTPS(w.ChartRepository) && summaryToken(w.Chart) && hbnVersionPattern.MatchString(w.ChartVersion) &&
		summaryDigest(w.ChartDigest) && hbnImageRepoPattern.MatchString(w.ImageRepository) && hbnTagPattern.MatchString(w.ImageTag) &&
		summaryDigest(w.ImageDigest) && hbnStartupPattern.MatchString(w.StartupVersion)
}

func validTenantKubernetesVersion(value string) bool {
	if !strings.HasPrefix(value, "v") || strings.ContainsAny(value, "-+") || strings.Count(value, ".") != 2 {
		return false
	}
	parsed, err := k8sversion.ParseSemantic(value)
	return err == nil && parsed.Major() > 0 && parsed.String() == strings.TrimPrefix(value, "v")
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
