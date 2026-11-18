// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	dpfassets "github.com/nvidia/k8s-launch-kit/profiles"
	yaml "gopkg.in/yaml.v3"
)

// ProvisioningDevice is the non-secret identity needed to select maintained
// DPF provisioning defaults. Model must be an exact catalog model and PSID
// must be observed for that device.
type ProvisioningDevice struct {
	Model string
	PSID  string
}

type hbnFlavorProfileLoader func(name string, networking hbn.Profile) (map[string]any, error)

// ResolveProvisioningDefaults combines site-owned allocations and HBN options
// with release-owned software and flavor defaults. It never reads files,
// credentials, inventory sidecars, or remote state. Missing catalog coverage is
// returned as issues so discovery can retain physical inventory while
// generation remains blocked.
func ResolveProvisioningDefaults(site Site, selected []ProvisioningDevice) (*Provisioning, []ProvisioningIssue, error) {
	intent, err := ResolveIntent(site)
	if err != nil {
		return nil, nil, err
	}
	return resolveProvisioningDefaults(intent, site, selected, loadHBNFlavorProfile)
}

func resolveProvisioningDefaults(intent Intent, site Site, selected []ProvisioningDevice, loadFlavor hbnFlavorProfileLoader) (*Provisioning, []ProvisioningIssue, error) {
	if intent.UseCase != releases.UseCaseHBNBGP {
		return nil, nil, fmt.Errorf("provisioning defaults require --use-case %s", releases.UseCaseHBNBGP)
	}
	if site.Provisioning != nil {
		if len(site.Provisioning.Flavor) > 0 {
			return nil, nil, fmt.Errorf("/provisioning/flavor is maintained by the selected DPF release and cannot be supplied by the site")
		}
		if softwareSupplied(site.Provisioning.Software) {
			return nil, nil, fmt.Errorf("/provisioning/software is maintained by the selected DPF release and cannot be supplied by the site")
		}
	}
	resolved, err := cloneProvisioning(site.Provisioning)
	if err != nil {
		return nil, nil, err
	}
	resolved.Flavor = nil
	resolved.Software = Software{}

	if len(selected) == 0 {
		return resolved, []ProvisioningIssue{{Field: "/inventory/devices", ReasonCode: "DPF_DEVICE_IDENTITY_REQUIRED", Message: "Discovery must observe each selected DPU's exact model and PSID before release artifacts can be resolved."}}, nil
	}

	issues := make([]ProvisioningIssue, 0)
	var selectedSoftware *Software
	flavorNames := make(map[string]bool)
	for _, device := range selected {
		model := strings.TrimSpace(device.Model)
		if model == "" {
			issues = append(issues, ProvisioningIssue{Field: "/inventory/devices/model", ReasonCode: "DPF_MODEL_UNAVAILABLE", Message: "The selected DPU has no observed model; a family-only BOM row cannot select provisioning artifacts or a flavor."})
			continue
		}
		hardware, found, ambiguous := hardwareForModel(intent.Release, model)
		if ambiguous {
			issues = append(issues, ProvisioningIssue{Field: "/inventory/devices/model", ReasonCode: "DPF_MODEL_CATALOG_AMBIGUOUS", Message: fmt.Sprintf("DPF release %s has duplicate exact hardware BOM rows for model %q.", intent.Release.Version, model)})
			continue
		}
		if !found {
			message := fmt.Sprintf("DPF release %s has no exact hardware BOM row for model %q; provisioning defaults cannot be selected.", intent.Release.Version, model)
			for _, row := range intent.Release.Hardware {
				if len(row.Models) == 0 && strings.EqualFold(row.Family, "BlueField-4") && strings.HasPrefix(strings.ToUpper(model), "B4") {
					message = fmt.Sprintf("DPF release %s lists BlueField-4 only at family level and does not qualify exact model %q; provisioning defaults cannot be selected.", intent.Release.Version, model)
					break
				}
			}
			issues = append(issues, ProvisioningIssue{Field: "/inventory/devices/model", ReasonCode: "DPF_MODEL_NOT_CATALOGED", Message: message})
			continue
		}
		if hardware.BlueFieldSoftware == nil {
			issues = append(issues, ProvisioningIssue{Field: "/provisioning/software", ReasonCode: "DPF_SOFTWARE_NOT_CATALOGED", Message: fmt.Sprintf("DPF release %s has no BlueField software artifact tuple for model %q.", intent.Release.Version, model)})
			continue
		}
		psid, err := canonicalPSID(device.PSID)
		if err != nil {
			issues = append(issues, ProvisioningIssue{Field: "/inventory/devices/psid", ReasonCode: "DPF_PSID_UNAVAILABLE", Message: fmt.Sprintf("The observed PSID for model %q is missing or invalid; firmware cannot be selected.", model)})
			continue
		}
		software, artifactIssue := softwareForDevice(intent.Release, model, psid, *hardware.BlueFieldSoftware)
		if artifactIssue != nil {
			issues = append(issues, *artifactIssue)
			continue
		}
		if selectedSoftware == nil {
			selectedSoftware = &software
		} else if selectedSoftware.OSISO != software.OSISO {
			issues = append(issues, ProvisioningIssue{Field: "/provisioning/software/osISO", ReasonCode: "DPF_OS_ISO_SELECTION_AMBIGUOUS", Message: "Selected hardware resolves to different release OS ISOs; the release catalog must provide one compatible selection for the selected device set."})
		}
		if prior, exists := selectedSoftware.PLDM[psid]; exists && prior != software.PLDM[psid] {
			issues = append(issues, ProvisioningIssue{Field: "/provisioning/software/pldm/" + psid, ReasonCode: "DPF_PSID_ARTIFACT_SELECTION_AMBIGUOUS", Message: fmt.Sprintf("Selected models resolve different PLDM artifacts for PSID %s; the release catalog must provide one compatible artifact.", psid)})
		} else {
			selectedSoftware.PLDM[psid] = software.PLDM[psid]
		}
		if hardware.HBNFlavorProfile == "" {
			issues = append(issues, ProvisioningIssue{Field: "/provisioning/flavor", ReasonCode: "HBN_FLAVOR_NOT_CATALOGED", Message: fmt.Sprintf("DPF release %s has no maintained HBN DPUFlavor profile for exact model %q.", intent.Release.Version, model)})
		} else {
			flavorNames[hardware.HBNFlavorProfile] = true
		}
	}
	if len(issues) != 0 {
		return resolved, issues, nil
	}
	if selectedSoftware == nil || len(flavorNames) != 1 {
		return resolved, []ProvisioningIssue{{Field: "/provisioning", ReasonCode: "DPF_PROVISIONING_SELECTION_INCOMPLETE", Message: "The selected release and hardware did not resolve one complete software tuple and one fixed HBN flavor profile."}}, nil
	}
	flavorName := ""
	for name := range flavorNames {
		flavorName = name
	}
	if loadFlavor == nil {
		return resolved, nil, fmt.Errorf("HBN flavor profile loader is required")
	}
	if intent.Networking == nil {
		return resolved, nil, fmt.Errorf("resolved HBN networking profile is required")
	}
	flavor, err := loadFlavor(flavorName, *intent.Networking)
	if err != nil {
		return resolved, []ProvisioningIssue{{Field: "/provisioning/flavor", ReasonCode: "HBN_FLAVOR_PROFILE_UNAVAILABLE", Message: fmt.Sprintf("The release catalog selects HBN flavor profile %q, but that maintained profile cannot be loaded: %v", flavorName, err)}}, nil
	}
	if len(flavor) == 0 {
		return resolved, nil, fmt.Errorf("maintained HBN flavor profile %q is empty", flavorName)
	}
	resolved.Software = *selectedSoftware
	resolved.Flavor, err = cloneProvisioningFlavor(flavor)
	if err != nil {
		return nil, nil, err
	}
	return resolved, nil, nil
}

func softwareSupplied(software Software) bool {
	return software.OSISO.URL != "" || software.OSISO.SHA256 != "" || len(software.PLDM) != 0
}

func hardwareForModel(release releases.Release, model string) (*releases.HardwareBOM, bool, bool) {
	var match *releases.HardwareBOM
	for i := range release.Hardware {
		for _, candidate := range release.Hardware[i].Models {
			if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(model)) {
				if match != nil {
					return nil, false, true
				}
				match = &release.Hardware[i]
			}
		}
	}
	return match, match != nil, false
}

func softwareForDevice(release releases.Release, model, psid string, bom releases.BlueFieldSoftware) (Software, *ProvisioningIssue) {
	software := Software{PLDM: map[string]Artifact{}}
	software.OSISO = Artifact{URL: bom.OSISO.URL, SHA256: strings.TrimPrefix(bom.OSISO.Digest, "sha256:")}
	if bom.OSISO.URL == "" || bom.OSISO.Digest == "" {
		return Software{}, catalogArtifactIssue("/provisioning/software/osISO", release.Version, model, bom.OSISO)
	}
	var pldm *releases.Artifact
	for candidate, artifact := range bom.PLDMFwBundle {
		if strings.EqualFold(candidate, psid) {
			if pldm != nil {
				return Software{}, &ProvisioningIssue{Field: "/provisioning/software/pldm/" + psid, ReasonCode: "DPF_PSID_ARTIFACT_AMBIGUOUS", Message: fmt.Sprintf("DPF release %s contains duplicate case-insensitive PLDM entries for PSID %s.", release.Version, psid)}
			}
			copy := artifact
			pldm = &copy
		}
	}
	if pldm == nil {
		return Software{}, &ProvisioningIssue{Field: "/provisioning/software/pldm/" + psid, ReasonCode: "DPF_PSID_ARTIFACT_NOT_CATALOGED", Message: fmt.Sprintf("DPF release %s has no PLDM artifact entry for selected model %q and PSID %s.", release.Version, model, psid)}
	}
	if pldm.URL == "" || pldm.Digest == "" {
		return Software{}, catalogArtifactIssue("/provisioning/software/pldm/"+psid, release.Version, model, *pldm)
	}
	software.PLDM[psid] = Artifact{URL: pldm.URL, SHA256: strings.TrimPrefix(pldm.Digest, "sha256:")}
	validated, issues, err := ValidateProvisioningSoftware(&Provisioning{Software: software}, []string{psid})
	if err != nil {
		return Software{}, &ProvisioningIssue{Field: "/provisioning/software", ReasonCode: "DPF_ARTIFACT_CATALOG_INVALID", Message: err.Error()}
	}
	if len(issues) > 0 {
		return Software{}, &issues[0]
	}
	return validated, nil
}

func catalogArtifactIssue(field, releaseVersion, model string, artifact releases.Artifact) *ProvisioningIssue {
	if artifact.ReasonCode != "" {
		return &ProvisioningIssue{Field: field, ReasonCode: artifact.ReasonCode, Message: artifact.Message}
	}
	return &ProvisioningIssue{Field: field, ReasonCode: "DPF_ARTIFACT_COORDINATE_INCOMPLETE", Message: fmt.Sprintf("DPF release %s catalog entry for model %q is missing a URL or SHA-256 digest.", releaseVersion, model)}
}

func cloneProvisioning(in *Provisioning) (*Provisioning, error) {
	if in == nil {
		return &Provisioning{}, nil
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("copy site provisioning options: %w", err)
	}
	var out Provisioning
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("copy site provisioning options: %w", err)
	}
	return &out, nil
}

func cloneProvisioningFlavor(in map[string]any) (map[string]any, error) {
	data, err := yaml.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("copy maintained HBN flavor: %w", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("copy maintained HBN flavor: %w", err)
	}
	return out, nil
}

func loadHBNFlavorProfile(name string, networking hbn.Profile) (map[string]any, error) {
	data, err := dpfassets.DPFHBNBGPFlavor(name)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode maintained HBN flavor %q: %w", name, err)
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok || len(spec) == 0 {
		return nil, fmt.Errorf("maintained HBN flavor %q must contain a nonempty spec mapping", name)
	}
	ovs, ok := spec["ovs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("maintained HBN flavor %q has no OVS config", name)
	}
	script, ok := ovs["rawConfigScript"].(string)
	const mtuMarker = "__L8K_UPLINK_MTU__"
	if !ok || strings.Count(script, mtuMarker) != 1 {
		return nil, fmt.Errorf("maintained HBN flavor %q must contain exactly one uplink MTU marker", name)
	}
	ovs["rawConfigScript"] = strings.Replace(script, mtuMarker, strconv.Itoa(networking.UplinkMTU), 1)
	return spec, nil
}
