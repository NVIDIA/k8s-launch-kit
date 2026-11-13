// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net/netip"
)

// ClusterNetworkConfig supplies authoritative ranges for IPPool collision checks.
// Nil lists request live discovery; non-nil lists replace discovery for that class.
// Empty lists are rejected because they cannot establish an authoritative range.
type ClusterNetworkConfig struct {
	PodCIDRs     []string `yaml:"podCIDRs,omitempty"`
	ServiceCIDRs []string `yaml:"serviceCIDRs,omitempty"`
}

// ValidateClusterNetworkConfig validates optional IPv4/IPv6 range overrides.
func ValidateClusterNetworkConfig(c *ClusterNetworkConfig) error {
	if c == nil {
		return nil
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{"podCIDRs", c.PodCIDRs}, {"serviceCIDRs", c.ServiceCIDRs},
	} {
		if field.values != nil && len(field.values) == 0 {
			return fmt.Errorf("cluster.%s must not be empty when supplied; omit it to discover ranges", field.name)
		}
		for i, value := range field.values {
			p, err := netip.ParsePrefix(value)
			if err != nil || p.Addr().Is4In6() {
				return fmt.Errorf("cluster.%s[%d] must be an IPv4 or IPv6 CIDR, got %q", field.name, i, value)
			}
		}
	}
	return nil
}
