// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/require"
)

func TestSpectrumXCLIReleaseBoundaries(t *testing.T) {
	for _, tc := range []struct {
		ra, release string
		valid       bool
	}{
		{"RA2.3", "26.4", false},
		{"RA2.3", "26.7", true},
		{"RA2.3", "26.10", false},
		{"RA2.4", "26.7", false},
		{"RA2.4", "26.10", true},
		{"RA2.4", "26.11", true},
		{"RA2.4", "27.1", true},
	} {
		t.Run(tc.ra+"/"+tc.release, func(t *testing.T) {
			err := ValidateSpectrumXSyntax(&options.Options{
				SpectrumX: true, SPCXVersion: tc.ra, NetworkOperatorRelease: tc.release,
			})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "--network-operator-release")
			}
		})
	}
}
