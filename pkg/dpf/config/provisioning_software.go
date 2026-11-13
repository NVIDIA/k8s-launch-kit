// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

// Provisioning contains DPF software inputs. It is optional during discovery;
// generation can require the resolved artifacts for its selected devices.
type Provisioning struct {
	Flavor    map[string]any         `yaml:"flavor,omitempty" json:"flavor,omitempty"`
	Resources map[string]string      `yaml:"resources,omitempty" json:"resources,omitempty"`
	Software  Software               `yaml:"software" json:"software"`
	HBN       *hbn.GenerationOptions `yaml:"hbn,omitempty" json:"hbn,omitempty"`
	Bootstrap *BootstrapOptions      `yaml:"bootstrap,omitempty" json:"bootstrap,omitempty"`
}

// Software identifies the OS ISO and the DPU firmware bundle for each PSID.
// DPF consumes URLs; SHA256 locks the downloaded bytes for executor verification.
type Software struct {
	OSISO Artifact            `yaml:"osISO" json:"osISO"`
	PLDM  map[string]Artifact `yaml:"pldm" json:"pldm"`
}

// Artifact identifies bytes without embedding them in discovery inventory or
// a generated bundle.
type Artifact struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// ProvisioningIssue is a non-fatal missing-input result suitable for discovery
// reports. Malformed supplied values are returned as errors instead.
type ProvisioningIssue struct {
	Field      string `json:"field"`
	ReasonCode string `json:"reasonCode"`
}

var provisioningPSID = regexp.MustCompile(`(?i)^MT_[A-Z0-9]{1,29}$`)

// ValidateProvisioningSoftware returns a normalized deep copy and missing
// input issues for the selected PSIDs. An empty selectedPSIDs means identity
// scope is not yet available (as during early discovery); it is reported as an
// issue instead of rejecting the inventory. Supplied artifact syntax is still
// validated, and when selection is known the PLDM map must match it exactly.
func ValidateProvisioningSoftware(input *Provisioning, selectedPSIDs []string) (Software, []ProvisioningIssue, error) {
	var supplied Software
	if input != nil {
		supplied = input.Software
	}

	selection := make(map[string]bool, len(selectedPSIDs))
	for _, psid := range selectedPSIDs {
		canonical, err := canonicalPSID(psid)
		if err != nil {
			return Software{}, nil, fmt.Errorf("invalid selected PSID")
		}
		selection[canonical] = true
	}

	software := Software{OSISO: supplied.OSISO, PLDM: make(map[string]Artifact, len(supplied.PLDM))}
	if err := validateArtifactSyntax("/provisioning/software/osISO", supplied.OSISO); err != nil {
		return Software{}, nil, err
	}
	keys := make([]string, 0, len(supplied.PLDM))
	for psid := range supplied.PLDM {
		keys = append(keys, psid)
	}
	slices.Sort(keys)
	for _, psid := range keys {
		canonical, err := canonicalPSID(psid)
		if err != nil {
			return Software{}, nil, fmt.Errorf("invalid PLDM PSID key")
		}
		if _, exists := software.PLDM[canonical]; exists {
			return Software{}, nil, fmt.Errorf("duplicate PLDM PSID after case normalization")
		}
		if len(selection) > 0 && !selection[canonical] {
			return Software{}, nil, fmt.Errorf("PLDM artifact supplied for an unselected PSID")
		}
		artifact := supplied.PLDM[psid]
		if err := validateArtifactSyntax("/provisioning/software/pldm/"+canonical, artifact); err != nil {
			return Software{}, nil, err
		}
		software.PLDM[canonical] = artifact
	}

	issues := []ProvisioningIssue{}
	if artifactIncomplete(supplied.OSISO) {
		issues = append(issues, ProvisioningIssue{Field: "/provisioning/software/osISO", ReasonCode: "OS_ISO_ARTIFACT_MISSING"})
	}
	if len(selection) == 0 {
		issues = append(issues, ProvisioningIssue{Field: "/provisioning/software/pldm", ReasonCode: "SELECTED_PSID_SCOPE_UNAVAILABLE"})
	} else {
		selected := make([]string, 0, len(selection))
		for psid := range selection {
			selected = append(selected, psid)
		}
		slices.Sort(selected)
		for _, psid := range selected {
			artifact, exists := software.PLDM[psid]
			if !exists || artifactIncomplete(artifact) {
				issues = append(issues, ProvisioningIssue{Field: "/provisioning/software/pldm/" + psid, ReasonCode: "PLDM_ARTIFACT_MISSING"})
			}
		}
	}
	return software, issues, nil
}

func canonicalPSID(psid string) (string, error) {
	if !provisioningPSID.MatchString(psid) {
		return "", fmt.Errorf("malformed PSID")
	}
	return strings.ToUpper(psid), nil
}

func artifactIncomplete(artifact Artifact) bool {
	return artifact.URL == "" || artifact.SHA256 == ""
}

func validateArtifactSyntax(field string, artifact Artifact) error {
	if artifact.URL != "" {
		u, err := url.Parse(artifact.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(artifact.URL, "#") {
			return fmt.Errorf("%s URL must be credential-free HTTPS without query or fragment", field)
		}
	}
	if artifact.SHA256 != "" {
		digest, err := hex.DecodeString(artifact.SHA256)
		if err != nil || len(digest) != 32 || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
			return fmt.Errorf("%s SHA256 must be 64 lowercase hexadecimal characters", field)
		}
	}
	return nil
}
