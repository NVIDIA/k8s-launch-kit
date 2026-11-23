// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
)

// selectNVAIERelease resolves provisioning intent before hardware and canonical
// defaults can turn an omitted Network Operator selection into an explicit one.
func selectNVAIERelease(cfg *config.LaunchKitConfig, inputs configinput.Values, present config.PathSet) error {
	release := cfg.NVAIERelease
	for _, override := range inputs.Overrides {
		if override.Path == "nvaieRelease" {
			value, ok := override.Value.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return fmt.Errorf("--nvaie-release must not be empty")
			}
			release = value
		}
	}
	if release == "" {
		return nil
	}
	selected := ""
	explicit := present.Has("networkOperator.selectedRelease")
	if cfg.NetworkOperator != nil {
		selected = cfg.NetworkOperator.SelectedRelease
	}
	for _, request := range inputs.Requests {
		if request.Kind == configinput.ResolverNetworkOperatorRelease {
			value, ok := request.Value.(string)
			if !ok {
				return fmt.Errorf("networkOperator release must be a string")
			}
			selected, explicit = value, true
		}
	}
	line, err := nvaieNetworkOperatorLine(release)
	if err != nil {
		return err
	}
	if explicit && selected != line {
		return fmt.Errorf("nvaieRelease %q requires networkOperator.selectedRelease %q, conflicts with explicit %q", release, line, selected)
	}
	if cfg.NetworkOperator == nil {
		cfg.NetworkOperator = &config.NetworkOperatorConfig{}
	}
	cfg.NVAIERelease = release
	cfg.NetworkOperator.SelectedRelease = line
	present.Add("networkOperator.selectedRelease")
	return nil
}

func nvaieNetworkOperatorLine(release string) (string, error) {
	entry, ok := nvaie.Lookup(release)
	if !ok {
		return "", fmt.Errorf("unsupported nvaieRelease %q; reviewed releases: %v", release, nvaie.SupportedReleases())
	}
	expected := entry.ComponentVersion(nvaie.NetworkOperator)
	version, err := semver.NewVersion(expected)
	if err != nil {
		return "", fmt.Errorf("NVAIE %s has invalid Network Operator pin %q", release, expected)
	}
	line := fmt.Sprintf("%d.%d", version.Major(), version.Minor())
	catalog, ok := releases.LookupRelease(line)
	if !ok {
		return "", fmt.Errorf("NVAIE %s requires unavailable Network Operator catalog line %s", release, line)
	}
	if strings.TrimPrefix(catalog.NetworkOperator.Version, "v") != strings.TrimPrefix(expected, "v") {
		return "", fmt.Errorf("NVAIE %s requires exact Network Operator %s; catalog %s contains %s", release, expected, line, catalog.NetworkOperator.Version)
	}
	doca := entry.ComponentVersion(nvaie.DOCADriver)
	// The Network Operator catalog owns the driver image build; NVAIE owns its
	// product release. Preserve the catalog's build suffix rather than inventing it.
	if doca == "" || !strings.HasPrefix(catalog.DOCADriver.Version, "doca"+strings.TrimPrefix(doca, "v")+"-") {
		return "", fmt.Errorf("NVAIE %s requires DOCA product %s; Network Operator catalog contains %s", release, doca, catalog.DOCADriver.Version)
	}
	return line, nil
}

// ValidateNVAIECohort checks durable intent on resolved and effective inputs.
// It never substitutes a release after an explicit incompatible selection.
func ValidateNVAIECohort(cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if cfg.NVAIERelease == "" {
		return nil
	}
	line, err := nvaieNetworkOperatorLine(cfg.NVAIERelease)
	if err != nil {
		return err
	}
	if cfg.NetworkOperator == nil || cfg.NetworkOperator.SelectedRelease != line {
		return fmt.Errorf("NVAIE %s requires Network Operator release line %s", cfg.NVAIERelease, line)
	}
	catalog, _ := releases.LookupRelease(line)
	if cfg.NetworkOperator.Version != catalog.NetworkOperator.Version || cfg.DOCADriver == nil || cfg.DOCADriver.Version != catalog.DOCADriver.Version {
		return fmt.Errorf("NVAIE %s resolved coordinates differ from the exact Network Operator/DOCA catalog cohort", cfg.NVAIERelease)
	}
	return nil
}
