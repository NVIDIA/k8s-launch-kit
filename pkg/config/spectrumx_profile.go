// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package config

import (
	"encoding/base64"
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	SpectrumXProfileLabel            = "network.nvidia.com/operator.nic-configuration.spectrum-x-profile"
	SpectrumXProfileConfigMapDataKey = "profile"
	DospcxDataFormat                 = "dospcx-data.tar.gz/v1"
	DospcxArchiveKey                 = "dospcx-data.tar.gz"
)

// SpectrumXProfileConfigRequired reports whether the RA version must be
// supplied to NIC Configuration Operator via a profile ConfigMap.
func SpectrumXProfileConfigRequired(ra string) bool {
	switch ra {
	case "", "RA2.1", "RA2.2":
		return false
	default:
		return true
	}
}

// NormalizeSpectrumXProfileConfig accepts a legacy profile ConfigMap or raw
// data.profile body, or a full doSPCX data-bundle ConfigMap. Legacy inputs are
// normalized to the profile body; doSPCX inputs retain their complete manifest.
// Full ConfigMaps contribute metadata.name; rendering supplies the resolved
// operator namespace and required label. RA compatibility is validated later.
func NormalizeSpectrumXProfileConfig(spcx *ProfileSpectrumX) error {
	if spcx == nil {
		return nil
	}

	if topologyType := strings.TrimSpace(spcx.TopologyType); topologyType != "" {
		normalized, err := NormalizeSpectrumXTopologyType(topologyType)
		if err != nil {
			return err
		}
		spcx.TopologyType = normalized
	}
	if ipVersion := strings.TrimSpace(spcx.IPVersion); ipVersion != "" {
		normalized, err := NormalizeSpectrumXIPVersion(ipVersion)
		if err != nil {
			return err
		}
		spcx.IPVersion = normalized
	}

	if strings.TrimSpace(spcx.Profile) == "" {
		return nil
	}

	raw := spcx.Profile
	cm, ok, err := parseSpectrumXProfileConfigMap(raw)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if strings.TrimSpace(cm.Metadata.Name) == "" {
		return fmt.Errorf("spectrum-x profile ConfigMap input is missing metadata.name")
	}
	if cm.Data["format"] == "" && len(cm.BinaryData) == 0 && strings.TrimSpace(cm.Data[SpectrumXProfileConfigMapDataKey]) == "" {
		return fmt.Errorf("spectrum-x profile ConfigMap input is missing non-empty data.%s", SpectrumXProfileConfigMapDataKey)
	}
	if spcx.ConfigMapName != "" && spcx.ConfigMapName != cm.Metadata.Name {
		return fmt.Errorf("profile.spectrumX.configMapName %q does not match input ConfigMap metadata.name %q",
			spcx.ConfigMapName, cm.Metadata.Name)
	}

	spcx.ConfigMapName = cm.Metadata.Name
	if cm.Data["format"] == "" && len(cm.BinaryData) == 0 {
		spcx.Profile = cm.Data[SpectrumXProfileConfigMapDataKey]
	}
	return nil
}

func NormalizeSpectrumXTopologyType(topologyType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(topologyType)) {
	case SpectrumXTopology2Tier, "2-tier-poc":
		return SpectrumXTopology2Tier, nil
	case SpectrumXTopology3Tier:
		return SpectrumXTopology3Tier, nil
	default:
		return "", fmt.Errorf("topologyType must be one of: %s, %s",
			SpectrumXTopology2Tier, SpectrumXTopology3Tier)
	}
}

func NormalizeSpectrumXIPVersion(ipVersion string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(ipVersion)) {
	case SpectrumXIPVersionIPv4:
		return SpectrumXIPVersionIPv4, nil
	case SpectrumXIPVersionIPv6:
		return SpectrumXIPVersionIPv6, nil
	default:
		return "", fmt.Errorf("ipVersion must be one of: %s, %s",
			SpectrumXIPVersionIPv4, SpectrumXIPVersionIPv6)
	}
}

func SpectrumXDefaultHostFirstOctet(topologyType string) int {
	if topologyType == SpectrumXTopology3Tier {
		return 10
	}
	return 172
}

type spectrumXProfileConfigMap struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace,omitempty"`
		Labels      map[string]string `yaml:"labels,omitempty"`
		Annotations map[string]string `yaml:"annotations,omitempty"`
	} `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
	BinaryData map[string]string `yaml:"binaryData,omitempty"`
}

func parseSpectrumXProfileConfigMap(raw string) (*spectrumXProfileConfigMap, bool, error) {
	var cm spectrumXProfileConfigMap
	if err := yaml.Unmarshal([]byte(raw), &cm); err != nil {
		return nil, false, fmt.Errorf("failed to parse spectrum-x profile input: %w", err)
	}

	if cm.Kind == "" {
		return nil, false, nil
	}
	if !strings.EqualFold(cm.Kind, "ConfigMap") {
		return nil, false, fmt.Errorf("spectrum-x profile input kind must be ConfigMap, got %q", cm.Kind)
	}
	return &cm, true, nil
}

// ValidateSpectrumXProfileFormat runs after final RA/CLI precedence is resolved.
func ValidateSpectrumXProfileFormat(spcx *ProfileSpectrumX) error {
	if spcx == nil {
		return nil
	}
	cm, full, err := parseSpectrumXProfileConfigMap(spcx.Profile)
	if err != nil {
		return err
	}
	if spcx.SPCXVersion != "RA2.4" {
		if full && (cm.Data["format"] != "" || len(cm.BinaryData) > 0) {
			return fmt.Errorf("doSPCX ConfigMap requires RA2.4")
		}
		return nil
	}
	if !full || cm.APIVersion != "v1" || cm.Kind != "ConfigMap" || cm.Data["format"] != DospcxDataFormat || cm.Metadata.Name == "" {
		return fmt.Errorf("RA2.4 requires a full v1 ConfigMap with data.format: %s", DospcxDataFormat)
	}
	if cm.Metadata.Name != spcx.ConfigMapName {
		return fmt.Errorf("doSPCX ConfigMap name %q does not match configMapName %q", cm.Metadata.Name, spcx.ConfigMapName)
	}
	archive, err := base64.StdEncoding.DecodeString(cm.BinaryData[DospcxArchiveKey])
	if err != nil || len(archive) == 0 {
		return fmt.Errorf("RA2.4 requires nonempty base64 binaryData.%s", DospcxArchiveKey)
	}
	return validateDospcxArchive(archive)
}

// RenderDospcxConfigMap preserves data/provenance but owns namespace and selector label.
func RenderDospcxConfigMap(spcx *ProfileSpectrumX, namespace string) (string, error) {
	if spcx == nil || spcx.SPCXVersion != "RA2.4" {
		return "", fmt.Errorf("RA2.4 doSPCX profile is required")
	}
	if err := ValidateSpectrumXProfileFormat(spcx); err != nil {
		return "", err
	}
	cm, _, err := parseSpectrumXProfileConfigMap(spcx.Profile)
	if err != nil {
		return "", err
	}
	cm.Metadata.Namespace = namespace
	if cm.Metadata.Labels == nil {
		cm.Metadata.Labels = map[string]string{}
	}
	cm.Metadata.Labels[SpectrumXProfileLabel] = ""
	data, err := yaml.Marshal(cm)
	return string(data), err
}
