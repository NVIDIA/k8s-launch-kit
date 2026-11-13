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

package profiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	launchtarget "github.com/nvidia/k8s-launch-kit/pkg/target"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ProfileRequirements struct {
	Flavor     string                        `yaml:"flavor,omitempty"`
	Fabric     string                        `yaml:"fabric"`
	Deployment string                        `yaml:"deployment"`
	Multirail  *bool                         `yaml:"multirail"`
	SpectrumX  *ProfileRequirementsSpectrumX `yaml:"spectrumX,omitempty"`
	// MinNetworkOperatorRelease is the lowest --network-operator-release this
	// profile supports (MAJOR.MINOR or full semver, e.g. "26.4"). When non-empty,
	// Validate rejects the profile if the user has selected a lower release.
	// An unset selectedRelease ("latest") always satisfies the minimum.
	MinNetworkOperatorRelease string `yaml:"minNetworkOperatorRelease,omitempty"`
	// MaxNetworkOperatorRelease is the highest --network-operator-release this
	// profile supports (MAJOR.MINOR or full semver, e.g. "26.1"). When non-empty,
	// Validate rejects the profile if the user has selected a higher release.
	// An unset selectedRelease ("latest") always satisfies the maximum. Use
	// together with MinNetworkOperatorRelease (set to the same value) to pin a
	// profile to a single release line.
	MaxNetworkOperatorRelease string `yaml:"maxNetworkOperatorRelease,omitempty"`
}

type ProfileRequirementsSpectrumX struct {
	SPCXVersion    string   `yaml:"spcxVersion"`              // Required version, e.g., "RA2.2"
	MultiplaneMode []string `yaml:"multiplaneMode,omitempty"` // Allowed multiplane modes, e.g., ["hwplb", "none"]
}

type NodeCapabilities struct {
	Sriov *bool `yaml:"sriov"`
	Rdma  *bool `yaml:"rdma"`
	Ib    *bool `yaml:"ib"`
}

type Profile struct {
	Name                string
	Plugin              string
	Description         string
	ProfileRequirements ProfileRequirements `yaml:"profileRequirements"`
	NodeCapabilities    NodeCapabilities    `yaml:"nodeCapabilities"`
	DeploymentGuide     string
	Templates           []string
}

// ProfileMetadata contains fields shared by all profile targets. Manifest is
// the original YAML so a target can decode its own payload without using the
// Host profile schema. TemplatePaths are resolved, existing files contained
// within the profiles root.
type ProfileMetadata struct {
	Target          launchtarget.Name
	Directory       string
	Name            string
	Plugin          string
	Description     string
	DeploymentGuide string
	TemplatePaths   []string
	Manifest        []byte
}

type profileMetadataYAML struct {
	Name            string   `yaml:"name"`
	Plugin          string   `yaml:"plugin"`
	Description     string   `yaml:"description"`
	DeploymentGuide string   `yaml:"deploymentGuide"`
	Templates       []string `yaml:"templates"`
}

// getprofilesDir resolves the profiles directory using a lookup chain:
// 1. ./profiles (CWD — container/repo root)
// 2. /usr/local/share/l8k/profiles (default install)
// 3. <binary-dir>/../share/l8k/profiles (custom prefix install)
func getprofilesDir() (string, error) {
	candidates := []string{
		"profiles",
		"/usr/local/share/l8k/profiles",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "share", "l8k", "profiles"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", apperrors.NewValidationError(
		"profiles directory not found",
		fmt.Errorf("checked: %s", strings.Join(candidates, ", ")),
		"Run 'scripts/install.sh' to install l8k, or run from the repository root")
}

// LoadProfileForTarget loads target-independent metadata and template paths for
// the exact profile directory name. The returned manifest is raw YAML for the
// target implementation to decode using its own schema.
func LoadProfileForTarget(targetName launchtarget.Name, profileName string) (*ProfileMetadata, error) {
	profilesDir, err := getprofilesDir()
	if err != nil {
		return nil, err
	}
	return loadProfileForTarget(profilesDir, targetName, profileName)
}

func loadProfileForTarget(profilesDir string, targetName launchtarget.Name, profileName string) (*ProfileMetadata, error) {
	if !knownProfileTarget(targetName) {
		return nil, fmt.Errorf("unknown profile target %q", targetName)
	}
	root, directory, manifest, err := readProfileManifest(profilesDir, profileName)
	if err != nil {
		return nil, err
	}
	declaredTarget, err := decodeProfileTarget(manifest)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", profileName, err)
	}
	if declaredTarget != targetName {
		return nil, fmt.Errorf("profile %q targets %q, not %q", profileName, declaredTarget, targetName)
	}
	return profileMetadataFromManifest(root, directory, profileName, declaredTarget, manifest)
}

func readProfileManifest(profilesDir, profileName string) (string, string, []byte, error) {
	if profileName == "" || profileName == "." || filepath.Base(profileName) != profileName || strings.ContainsAny(profileName, `/\\`) {
		return "", "", nil, fmt.Errorf("invalid profile name %q", profileName)
	}

	root, err := filepath.EvalSymlinks(profilesDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve profiles directory: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve profiles directory: %w", err)
	}
	profilePath := filepath.Join(root, profileName)
	info, err := os.Stat(profilePath)
	if err != nil {
		return "", "", nil, fmt.Errorf("profile %q not found: %w", profileName, err)
	}
	if !info.IsDir() {
		return "", "", nil, fmt.Errorf("profile %q is not a directory", profileName)
	}
	directory, err := resolveContainedPath(root, root, profileName)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve profile %q: %w", profileName, err)
	}

	manifestPath, err := resolveContainedPath(root, directory, "profile.yaml")
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve profile %q manifest: %w", profileName, err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("read profile %q manifest: %w", profileName, err)
	}
	return root, directory, manifest, nil
}

func profileMetadataFromManifest(root, directory, profileName string, declaredTarget launchtarget.Name, manifest []byte) (*ProfileMetadata, error) {
	var metadata profileMetadataYAML
	if err := yaml.Unmarshal(manifest, &metadata); err != nil {
		return nil, fmt.Errorf("decode profile %q metadata: %w", profileName, err)
	}

	templatePaths := make([]string, 0, len(metadata.Templates))
	for _, template := range metadata.Templates {
		if template == "" {
			return nil, fmt.Errorf("profile %q contains an empty template path", profileName)
		}
		path, err := resolveContainedPath(root, directory, filepath.FromSlash(template))
		if err != nil {
			return nil, fmt.Errorf("profile %q template %q: %w", profileName, template, err)
		}
		if info, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("profile %q template %q: %w", profileName, template, err)
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("profile %q template %q is not a regular file", profileName, template)
		}
		templatePaths = append(templatePaths, path)
	}

	return &ProfileMetadata{
		Target:          declaredTarget,
		Directory:       directory,
		Name:            metadata.Name,
		Plugin:          metadata.Plugin,
		Description:     metadata.Description,
		DeploymentGuide: metadata.DeploymentGuide,
		TemplatePaths:   templatePaths,
		Manifest:        manifest,
	}, nil
}

func decodeProfileTarget(manifest []byte) (launchtarget.Name, error) {
	var header yaml.MapSlice
	if err := yaml.Unmarshal(manifest, &header); err != nil {
		return "", fmt.Errorf("decode target header: %w", err)
	}
	var declared string
	found := false
	for _, item := range header {
		key, ok := item.Key.(string)
		if !ok || key != "target" {
			continue
		}
		if found {
			return "", fmt.Errorf("duplicate target header")
		}
		found = true
		value, ok := item.Value.(string)
		if !ok {
			return "", fmt.Errorf("invalid target header: expected a string")
		}
		declared = value
	}
	if !found {
		return launchtarget.Host, nil
	}
	targetName := launchtarget.Name(declared)
	if !knownProfileTarget(targetName) {
		return "", fmt.Errorf("unknown or invalid target %q", declared)
	}
	return targetName, nil
}

func knownProfileTarget(targetName launchtarget.Name) bool {
	return targetName == launchtarget.Host || targetName == launchtarget.DPF
}

// resolveContainedPath follows symlinks but rejects any result outside root.
// The profile root itself may be a symlink in an installation layout.
func resolveContainedPath(root, base, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	candidate := filepath.Join(base, relative)
	lexical, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if err := ensureContained(root, lexical); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if err := ensureContained(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func ensureContained(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes profiles directory")
	}
	return nil
}

// FindApplicableProfile selects a profile from disk that matches the user's
// requirements, cluster capabilities, and selected Network Operator release.
// selectedRelease is the catalog key (MAJOR.MINOR, e.g. "26.4") chosen via
// --network-operator-release; an empty string means "no release pinned" and
// disables the minimum-release check.
func FindApplicableProfile(requirements *config.Profile, capabilities *config.ClusterCapabilities, pluginName, selectedRelease, flavor string) (*Profile, error) {
	if flavor == "" {
		flavor = config.FlavorK8s
	}
	log.Log.Info("Finding applicable profile", "requirements", requirements, "flavor", flavor)

	profilesDir, err := getprofilesDir()
	if err != nil {
		return nil, err
	}
	return findApplicableProfile(profilesDir, requirements, capabilities, pluginName, selectedRelease, flavor)
}

func findApplicableProfile(profilesDir string, requirements *config.Profile, capabilities *config.ClusterCapabilities, pluginName, selectedRelease, flavor string) (*Profile, error) {
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil, err
	}

	log.Log.V(1).Info("Found profiles", "count", len(entries))

	errorMessages := []string{}

	for _, entry := range entries {
		if entry.IsDir() {
			root, directory, manifest, err := readProfileManifest(profilesDir, entry.Name())
			if err != nil {
				log.Log.Error(err, "failed to read profile manifest", "profile", entry.Name())
				return nil, err
			}
			profileTarget, err := decodeProfileTarget(manifest)
			if err != nil {
				return nil, fmt.Errorf("profile %q: %w", entry.Name(), err)
			}
			if profileTarget != launchtarget.Host {
				continue
			}
			metadata, err := profileMetadataFromManifest(root, directory, entry.Name(), profileTarget, manifest)
			if err != nil {
				return nil, err
			}
			profile := &Profile{}
			err = yaml.Unmarshal(metadata.Manifest, profile)
			if err != nil {
				log.Log.Error(err, "failed to unmarshal Host profile manifest", "profile", entry.Name())
				return nil, err
			}
			if profile.Plugin != pluginName {
				continue
			}
			profileFlavor := profile.ProfileRequirements.Flavor
			if profileFlavor == "" {
				profileFlavor = config.FlavorK8s
			}
			if profileFlavor != flavor {
				continue
			}
			valid, reason := profile.Validate(requirements, capabilities, selectedRelease)
			if valid {
				log.Log.V(1).Info("Found applicable profile", "profile", profile)
				profile.Templates = metadata.TemplatePaths
				profile.DeploymentGuide = filepath.Join(metadata.Directory, profile.DeploymentGuide)
				return profile, nil
			} else {
				errorMessages = append(errorMessages, fmt.Sprintf("profile %s is not applicable: %s", entry.Name(), reason))
			}
		}
	}

	log.Log.Info("No applicable profile found based on the given requirements")
	for _, errorMessage := range errorMessages {
		log.Log.Error(errors.New(errorMessage), "errorMessage")
	}

	return nil, apperrors.NewValidationError(
		"no applicable profile found",
		fmt.Errorf("tried %d profiles, none matched: %s", len(errorMessages), strings.Join(errorMessages, "; ")),
		fmt.Sprintf("Check --flavor %s, --fabric, --deployment-type, and --spectrum-x flags", flavor))
}

// Validate reports whether the profile applies for the given requirements,
// cluster capabilities, and selected Network Operator release. selectedRelease
// is the catalog key (MAJOR.MINOR, e.g. "26.4"); empty means "no release
// pinned" and disables the minimum-release gate.
func (p *Profile) Validate(requirements *config.Profile, capabilities *config.ClusterCapabilities, selectedRelease string) (bool, string) {
	log.Log.V(1).Info("Validating profile", "profile", p)

	if p.ProfileRequirements.MinNetworkOperatorRelease != "" && selectedRelease != "" {
		if releaseLessThan(selectedRelease, p.ProfileRequirements.MinNetworkOperatorRelease) {
			return false, fmt.Sprintf("profile requires Network Operator >= %s, got %s",
				p.ProfileRequirements.MinNetworkOperatorRelease, selectedRelease)
		}
	}

	if p.ProfileRequirements.MaxNetworkOperatorRelease != "" && selectedRelease != "" {
		if releaseGreaterThan(selectedRelease, p.ProfileRequirements.MaxNetworkOperatorRelease) {
			return false, fmt.Sprintf("profile requires Network Operator <= %s, got %s",
				p.ProfileRequirements.MaxNetworkOperatorRelease, selectedRelease)
		}
	}

	if p.ProfileRequirements.Fabric != "" && p.ProfileRequirements.Fabric != requirements.Fabric {
		return false, fmt.Sprintf("selected fabric type does not match profile requirements: %s", p.ProfileRequirements.Fabric)
	}

	if p.ProfileRequirements.Deployment != "" && p.ProfileRequirements.Deployment != requirements.Deployment {
		return false, fmt.Sprintf("selected deployment type does not match profile requirements: %s", p.ProfileRequirements.Deployment)
	}

	if requirements.Fabric == "infiniband" && !capabilities.Nodes.Ib {
		return false, "selected InfiniBand fabric requires cluster ib capability to be true"
	}

	if p.ProfileRequirements.Multirail != nil && *p.ProfileRequirements.Multirail != requirements.Multirail {
		return false, fmt.Sprintf("selected multirail setting does not match profile requirements: %t", *p.ProfileRequirements.Multirail)
	}

	// Spectrum-X is a strict, explicit opt-in. If the user enabled it, only
	// profiles whose requirements declare Spectrum-X may match — otherwise we
	// silently fall through to a non-Spectrum-X profile (e.g. sriov-ethernet-rdma)
	// and the user gets manifests that don't deploy the SR-IOV operator chain
	// or the SpectrumXRailPoolConfig they expected.
	if p.ProfileRequirements.SpectrumX == nil &&
		requirements.SpectrumX != nil && requirements.SpectrumX.Enable {
		return false, "user requested Spectrum-X but this profile is not a Spectrum-X profile"
	}

	if p.ProfileRequirements.SpectrumX != nil {
		// Profile requires Spectrum-X with enable: true
		if requirements.SpectrumX == nil || !requirements.SpectrumX.Enable {
			return false, "profile requires Spectrum-X but it is not enabled"
		}

		// Validate SPCX version if specified in profile requirements
		if p.ProfileRequirements.SpectrumX.SPCXVersion != "" {
			if requirements.SpectrumX.SPCXVersion != p.ProfileRequirements.SpectrumX.SPCXVersion {
				return false, fmt.Sprintf("profile requires SPCX version %s but got %s",
					p.ProfileRequirements.SpectrumX.SPCXVersion, requirements.SpectrumX.SPCXVersion)
			}
		}

		// Validate multiplane mode if specified in profile requirements
		if len(p.ProfileRequirements.SpectrumX.MultiplaneMode) > 0 {
			modeMatch := false
			for _, mode := range p.ProfileRequirements.SpectrumX.MultiplaneMode {
				if mode == requirements.SpectrumX.MultiplaneMode {
					modeMatch = true
					break
				}
			}
			if !modeMatch {
				return false, fmt.Sprintf("multiplane mode %s not in profile's allowed modes %v",
					requirements.SpectrumX.MultiplaneMode, p.ProfileRequirements.SpectrumX.MultiplaneMode)
			}
		}
	}

	if p.NodeCapabilities.Sriov != nil && *p.NodeCapabilities.Sriov != capabilities.Nodes.Sriov {
		return false, fmt.Sprintf("cluster sriov capability does not match profile requirements: %t", *p.NodeCapabilities.Sriov)
	}
	if p.NodeCapabilities.Rdma != nil && *p.NodeCapabilities.Rdma != capabilities.Nodes.Rdma {
		return false, fmt.Sprintf("cluster rdma capability does not match profile requirements: %t", *p.NodeCapabilities.Rdma)
	}
	if p.NodeCapabilities.Ib != nil && *p.NodeCapabilities.Ib != capabilities.Nodes.Ib {
		return false, fmt.Sprintf("cluster ib capability does not match profile requirements: %t", *p.NodeCapabilities.Ib)
	}

	return true, ""
}

// releaseLessThan compares two release identifiers (MAJOR.MINOR or full
// semver). Returns false on parse failure so a malformed value never silently
// disqualifies a profile. "26.4" is treated as "26.4.0".
func releaseLessThan(have, target string) bool {
	h, t, ok := parseReleases(have, target)
	if !ok {
		return false
	}
	return h.LessThan(t)
}

// releaseGreaterThan is the symmetric counterpart of releaseLessThan, used
// for the MaxNetworkOperatorRelease upper bound. Same parse-failure
// semantics: a malformed value never silently disqualifies a profile.
func releaseGreaterThan(have, target string) bool {
	h, t, ok := parseReleases(have, target)
	if !ok {
		return false
	}
	return h.GreaterThan(t)
}

func parseReleases(have, target string) (*semver.Version, *semver.Version, bool) {
	normalize := func(s string) string {
		s = strings.TrimPrefix(s, "v")
		if strings.Count(s, ".") == 1 {
			return s + ".0"
		}
		return s
	}
	h, err := semver.NewVersion(normalize(have))
	if err != nil {
		return nil, nil, false
	}
	t, err := semver.NewVersion(normalize(target))
	if err != nil {
		return nil, nil, false
	}
	return h, t, true
}

// UpdateManifestsPaths appends the directory path to the templates and deployment guide
func (p *Profile) UpdateManifestsPaths(dirPath string) {
	for i := range p.Templates {
		p.Templates[i] = filepath.Join(dirPath, p.Templates[i])
	}

	p.DeploymentGuide = filepath.Join(dirPath, p.DeploymentGuide)
}
