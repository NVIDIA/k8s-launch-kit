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
	resolved, err := generationInputs(input)
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
