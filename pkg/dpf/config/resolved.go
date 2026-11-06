// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Resolved is deployment metadata, not another user input/defaulting path.
type Resolved struct {
	FormatVersion string     `yaml:"formatVersion"`
	Target        string     `yaml:"target"`
	Config        Generation `yaml:"config"`
}

func EncodeResolved(g Generation) ([]byte, error) {
	if err := ValidateGeneration(g); err != nil {
		return nil, err
	}
	return yaml.Marshal(Resolved{FormatVersion: "v1alpha1", Target: "dpf", Config: g})
}

func DecodeResolved(b []byte) (Generation, error) {
	var r Resolved
	if len(b) > 16<<20 {
		return r.Config, fmt.Errorf("resolved DPF configuration exceeds 16 MiB")
	}
	var syntax yaml.Node
	if err := yaml.Unmarshal(b, &syntax); err != nil {
		return r.Config, fmt.Errorf("invalid resolved DPF YAML")
	}
	if err := validateKeys(&syntax, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}, 0); err != nil {
		return r.Config, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&r); err != nil {
		return r.Config, fmt.Errorf("invalid resolved DPF configuration: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return r.Config, fmt.Errorf("resolved DPF configuration must be one document")
	}
	if r.Target != "dpf" || r.FormatVersion != "v1alpha1" {
		return r.Config, fmt.Errorf("unsupported resolved DPF format or target")
	}
	return r.Config, ValidateGeneration(r.Config)
}

// ValidateGeneration checks the resolved deployment contract without defaulting,
// filesystem access, live discovery or credential resolution.
func ValidateGeneration(g Generation) error {
	if err := validateKubernetesVersion(g.ManagementKubernetesVersion); err != nil {
		return err
	}
	if err := validateKubernetesVersion(g.Provisioning.DPUCluster.KubernetesVersion); err != nil {
		return err
	}
	if g.Profile != "sentry-bf4" || !generationRelease(g.Release) {
		return fmt.Errorf("unsupported resolved DPF profile/release")
	}
	if !dnsLabel(g.Name) || len(g.Name) > 40 || !dnsLabel(g.Namespace) || len(g.Namespace) > 40 {
		return fmt.Errorf("invalid resolved DPF name or namespace")
	}
	if g.ManagementUID == "" || g.Context == "" || !filepath.IsAbs(g.KubeconfigRef.File) {
		return fmt.Errorf("resolved management identity and explicit absolute kubeconfig/context are required")
	}
	if g.Sentry.Namespace == nil || !dnsLabel(*g.Sentry.Namespace) || *g.Sentry.Namespace == g.Namespace {
		return fmt.Errorf("resolved Sentry namespace must be dedicated")
	}
	if g.Sentry.PolicyServerNodePort == nil || g.Sentry.LLMInspection == nil || !filepath.IsAbs(g.Sentry.ChartDirectory) {
		return fmt.Errorf("resolved Sentry port and absolute chart directory are required")
	}
	s := g.Sentry
	if err := resolveSentry(&s); err != nil {
		return err
	}
	if g.Provisioning.DPUCluster.ResourceUID == "" || g.Provisioning.DPUCluster.ClusterUID == "" || !dnsLabel(g.Provisioning.DPUCluster.Name) || !dnsLabel(g.Provisioning.DPUCluster.Namespace) || len(g.Provisioning.DPUCluster.Selector) == 0 {
		return fmt.Errorf("resolved DPUCluster identity/selector is incomplete")
	}
	for key, value := range g.Provisioning.DPUCluster.Selector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("invalid resolved DPUCluster selector")
		}
	}
	for psid, artifact := range g.Provisioning.Software.PLDM {
		if !strings.HasPrefix(psid, "MT_") || len(psid) > 32 {
			return fmt.Errorf("invalid resolved PLDM PSID")
		}
		if err := validateArtifact(artifact); err != nil {
			return err
		}
	}
	if len(g.Devices) == 0 || len(g.Devices) != len(g.Provisioning.Devices) || len(g.DevicePSIDs) != len(g.Devices) {
		return fmt.Errorf("resolved selected-device membership is incomplete")
	}
	ids, serials, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, d := range g.Devices {
		binding := g.Provisioning.Devices[i]
		if d.ID == "" || d.ID != binding.ID || ids[d.ID] || d.SerialNumber == "" || serials[d.SerialNumber] || names[binding.DPUDevice] || !dnsLabel(binding.DPUDevice) || !dnsLabel(binding.DPUNode) {
			return fmt.Errorf("resolved device identities or bindings conflict")
		}
		ids[d.ID], serials[d.SerialNumber], names[binding.DPUDevice] = true, true, true
		if d.Model != "BlueField-4" || d.ObservationState != "observed" {
			return fmt.Errorf("resolved device %s has no observed BF4 identity", d.ID)
		}
		if _, err := Origin(d.BMCEndpoint); err != nil {
			return err
		}
		psid := g.DevicePSIDs[d.ID]
		if psid == "" {
			return fmt.Errorf("resolved device %s has no PSID", d.ID)
		}
		a, ok := g.Provisioning.Software.PLDM[psid]
		if !ok {
			return fmt.Errorf("resolved device %s has no matching PLDM artifact", d.ID)
		}
		if err := validateArtifact(a); err != nil {
			return err
		}
	}
	if err := validateArtifact(g.Provisioning.Software.OSISO); err != nil {
		return err
	}
	if g.Provisioning.Software.NICFirmware != nil {
		return validateArtifact(*g.Provisioning.Software.NICFirmware)
	}
	return nil
}

// SameIntent allows an explicit config to win only when its effective desired
// deployment still matches the generated bundle. A changed intent needs generate,
// rather than silently applying manifests for the previous input.
func SameIntent(a, b Generation) bool {
	// Fresh observation timestamps and diagnostics are not desired configuration.
	identities := func(devices []inventory.Device) []inventory.Device {
		out := make([]inventory.Device, len(devices))
		for i, d := range devices {
			out[i] = inventory.Device{ID: d.ID, SerialNumber: d.SerialNumber, BMCEndpoint: d.BMCEndpoint, Model: d.Model, ObservationState: d.ObservationState}
		}
		return out
	}
	a.Devices, b.Devices = identities(a.Devices), identities(b.Devices)
	// Local access paths can move without changing resource intent; deploy uses the
	// explicit path only after its cluster identity has been checked live.
	a.KubeconfigRef, b.KubeconfigRef = FileRef{}, FileRef{}
	a.Sentry.ChartDirectory, b.Sentry.ChartDirectory = "", ""
	return reflect.DeepEqual(a, b)
}
