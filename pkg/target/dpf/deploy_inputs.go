// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// withRetainedConfiguration supplies the bundle's desired settings only for a
// fresh inventory without explicit intent. A supplied override file or saved
// discovery configuration must still match the retained plan before any apply.
func withRetainedConfiguration(input config.Input, plan *generation.Plan) (config.Input, error) {
	if !input.External || input.OverridesPath != "" || config.HasConfiguration(input) {
		return input, nil
	}
	if plan == nil {
		return config.Input{}, fmt.Errorf("deployment requires a retained bundle")
	}
	settings, err := plan.Configuration()
	if err != nil {
		return config.Input{}, err
	}
	return config.WithConfiguration(input, settings)
}

// withRetainedProvisioning supplies fixed validated implementation while
// preserving current allowed policy and environment inputs. External records
// remain public inventories; config.Saved never publishes these internals.
func withRetainedProvisioning(input config.Input, plan *generation.Plan) (config.Input, error) {
	provisioning, err := plan.ResolveProvisioning(input.Site)
	if err != nil {
		return config.Input{}, err
	}
	input.Site.Provisioning = provisioning
	return input, nil
}

// withRetainedUseCase restores the generation selection before resolving access
// or collecting fresh inventory. Bundle loading has checked structural integrity;
// Run still verifies the complete retained generation before external effects.
func withRetainedUseCase(input config.Input, retained *bundle.Bundle) (config.Input, error) {
	if retained == nil {
		return config.Input{}, fmt.Errorf("deployment requires a retained HBN-BGP bundle selection")
	}
	selection := retained.Index().Profile
	if selection != "hbn-bgp" {
		return config.Input{}, fmt.Errorf("retained bundle does not select the supported HBN-BGP use case")
	}
	if current := input.Site.DPF.UseCase; current != nil {
		if *current == selection {
			return input, nil
		}
		// An unchanged source value can have been overridden by generation's
		// explicit CLI selection. A new conflicting value must not be hidden.
		var source []byte
		for _, file := range retained.Files() {
			if file.Path == ".l8k/source-config.yaml" {
				source = file.Data
				break
			}
		}
		before, err := config.Decode(source, input.Path)
		if err != nil || before.Site.DPF.UseCase == nil || *before.Site.DPF.UseCase != *current {
			return config.Input{}, fmt.Errorf("current use-case selection differs from the generated selection and retained source; regenerate and review the bundle")
		}
	}
	// Preserve Input.Source as the original file guard. The effective document
	// and typed site carry this selection through every fresh prerequisite pass.
	return config.WithUseCase(input, &selection)
}

// verifyDeploymentPlan binds current site intent and physical observations to
// the retained desired bundle. Access references may change, but neither the
// generated resources nor the selected physical host bindings may change.
func verifyDeploymentPlan(input config.Input, retained *bundle.Bundle, plan *generation.Plan) error {
	if retained == nil || plan == nil || retained.Index().BundleDigest != plan.Digest() {
		return fmt.Errorf("deployment requires the validated retained bundle")
	}
	var source []byte
	for _, file := range retained.Files() {
		if file.Path == ".l8k/source-config.yaml" {
			source = file.Data
			break
		}
	}
	before, err := config.Decode(source, input.Path)
	if err != nil || before.Site.Inventory == nil {
		return fmt.Errorf("retained source lacks the discovery inventory; regenerate the bundle")
	}
	if err := verifyDeploymentPhysicalBindings(before.Site.Inventory, input.Site.Inventory); err != nil {
		return err
	}
	resolved, err := generationInputsForPlan(input, plan)
	if err != nil {
		return fmt.Errorf("current deployment inputs cannot resolve the retained intent: %w", err)
	}
	_, resolved.OperatorArchive = plan.Operator()
	resolved.HBNChartArchive = plan.HBNChart()
	// Source YAML is provenance, not desired resources. Retain its exact bytes
	// while checking that current facts and overrides render identical intent.
	resolved.SourceConfig = source
	current, err := generation.Generate(resolved.Inputs)
	if err != nil {
		return fmt.Errorf("current deployment inputs failed retained-schema validation: %w", err)
	}
	if current.Index().BundleDigest != plan.Digest() {
		return fmt.Errorf("current site inputs or discovered identities change the generated plan; regenerate and review the bundle before deploying")
	}
	return nil
}

func verifyDeploymentPhysicalBindings(before, current *inventory.Inventory) error {
	if before == nil || current == nil || before.FormatVersion != inventory.FormatVersion || current.FormatVersion != inventory.FormatVersion {
		return fmt.Errorf("retained and current discovery inventories are required")
	}
	if before.ScopeDigest == "" || before.ScopeDigest != current.ScopeDigest || inventory.IdentityConflict(before, current) {
		return fmt.Errorf("selected access scope, trust or physical identity changed since generation; rediscover and regenerate")
	}
	if len(before.DPUs) == 0 || len(before.DPUs) != len(current.DPUs) || len(before.Hosts) != len(current.Hosts) || len(before.Associations) != len(current.Associations) {
		return fmt.Errorf("selected host and DPU membership changed since generation")
	}
	previousDevices := append(append([]inventory.Device(nil), before.Hosts...), before.DPUs...)
	currentDevices := append(append([]inventory.Device(nil), current.Hosts...), current.DPUs...)
	seen := map[string]bool{}
	for _, previous := range previousDevices {
		if previous.InputRef == "" || seen[previous.InputRef] {
			return fmt.Errorf("retained physical source bindings are incomplete or duplicated")
		}
		seen[previous.InputRef] = true
		matches := 0
		for _, host := range currentDevices {
			if host.InputRef == previous.InputRef && host.ID == previous.ID && host.ID != "" &&
				host.SystemUUID == previous.SystemUUID && host.SerialNumber == previous.SerialNumber && host.SSHEndpoint == previous.SSHEndpoint &&
				host.BMCEndpoint == previous.BMCEndpoint && host.SystemURI == previous.SystemURI {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("a selected physical host or DPU binding changed since generation")
		}
	}
	seen = map[string]bool{}
	for _, previous := range before.Associations {
		if previous.DPUID == "" || previous.HostID == "" || previous.HostAdapterKey == "" || previous.SerialNumber == "" || seen[previous.DPUID] {
			return fmt.Errorf("retained DPU-to-host associations are incomplete or duplicated")
		}
		seen[previous.DPUID] = true
		matches := 0
		for _, association := range current.Associations {
			if association.DPUID == previous.DPUID && association.HostID == previous.HostID &&
				association.HostAdapterKey == previous.HostAdapterKey && association.SerialNumber == previous.SerialNumber {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("a selected DPU-to-host association changed since generation")
		}
	}
	return nil
}
