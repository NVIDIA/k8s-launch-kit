// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
)

// SPCXReleasePolicy defines the default and inclusive supported release bounds.
type SPCXReleasePolicy struct{ DefaultRelease, MinRelease, MaxRelease string }

var spcxReleasePolicies = map[string]SPCXReleasePolicy{
	"RA2.1": {"26.1", "26.1", "26.1"},
	"RA2.2": {"26.4", "26.4", "26.4"},
	"RA2.3": {"26.7", "26.7", "26.7"},
	"RA2.4": {DefaultRelease: "26.10", MinRelease: "26.10"},
}

// DefaultSPCXReleaseFor keeps the default independent of future catalog additions.
func DefaultSPCXReleaseFor(ra string) string { return spcxReleasePolicies[ra].DefaultRelease }

// ValidateSPCXRelease checks compatibility; catalog membership is enforced separately.
func ValidateSPCXRelease(ra, release string) error {
	policy, ok := spcxReleasePolicies[ra]
	if !ok {
		return fmt.Errorf("unknown SPC-X RA version %q", ra)
	}
	have, err := semver.NewVersion(release)
	if err != nil {
		return fmt.Errorf("invalid Network Operator release %q: %w", release, err)
	}
	min, err := semver.NewVersion(policy.MinRelease)
	if err != nil {
		return fmt.Errorf("invalid minimum release: %w", err)
	}
	aboveMax := false
	if policy.MaxRelease != "" {
		max, parseErr := semver.NewVersion(policy.MaxRelease)
		if parseErr != nil {
			return fmt.Errorf("invalid maximum release: %w", parseErr)
		}
		aboveMax = have.GreaterThan(max)
	}
	if have.LessThan(min) || aboveMax {
		if policy.MaxRelease != "" {
			return fmt.Errorf("--spectrum-x %s requires --network-operator-release in [%s], got %s", ra, policy.MinRelease, release)
		}
		return fmt.Errorf("--spectrum-x %s requires --network-operator-release >= %s, got %s", ra, policy.MinRelease, release)
	}
	return nil
}
