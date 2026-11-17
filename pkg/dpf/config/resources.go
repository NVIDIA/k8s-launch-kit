// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

// ResolveHBNResources overlays optional site quantities on the embedded HBN
// profile defaults. The HBN chart uses each quantity as both a request and a
// limit. Nil means omitted; a nonnil empty map is rejected. The result owns its
// map and does not retain caller or profile map storage.
func ResolveHBNResources(override map[string]string) (hbn.Resources, error) {
	definition, err := hbn.LoadDefinition()
	if err != nil {
		return nil, fmt.Errorf("load HBN resource defaults: %w", err)
	}
	resolved := cloneHBNResources(definition.Defaults.Resources)
	if override != nil {
		if len(override) == 0 {
			return nil, fmt.Errorf("provisioning.resources must not be empty")
		}
		for name, quantity := range override {
			resolved[name] = quantity
		}
	}
	if err := hbn.ValidateResources(resolved); err != nil {
		return nil, fmt.Errorf("invalid resolved HBN resources: %w", err)
	}
	return resolved, nil
}

func cloneHBNResources(in hbn.Resources) hbn.Resources {
	if in == nil {
		return nil
	}
	out := make(hbn.Resources, len(in))
	for name, quantity := range in {
		out[name] = quantity
	}
	return out
}
