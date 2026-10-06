// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeInputRejectsDuplicateSpectrumXSwPlaneKeys(t *testing.T) {
	for name, source := range map[string]string{
		"explicit":          "clusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      0: 0\n      0: 1\n",
		"numeric spellings": "clusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      0x0: 0\n      00: 1\n",
		"map alias":         "planes: &planes\n  0: 0\n  0: 1\nclusterConfig:\n- spectrumX:\n    swPlaneByRail: *planes\n",
		"group alias":       "group: &group\n  spectrumX:\n    swPlaneByRail:\n      0: 0\n      0: 1\nclusterConfig:\n- *group\n",
		"group merge":       "group: &group\n  spectrumX:\n    swPlaneByRail:\n      0: 0\n      0: 1\nclusterConfig:\n- <<: *group\n  identifier: machine-a\n",
		"spectrum-x merge":  "sx: &sx\n  swPlaneByRail:\n    0: 0\n    0: 1\nclusterConfig:\n- spectrumX:\n    <<: *sx\n",
		"rail-map merge":    "planes: &planes\n  0: 0\n  0: 1\nclusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      <<: *planes\n      1: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeInput([]byte(source), "planes.yaml")
			require.ErrorContains(t, err, "duplicate swPlaneByRail rail key 0")
			require.ErrorContains(t, err, "planes.yaml")
		})
	}
}

func TestDecodeInputPreservesSpectrumXSwPlaneMergeOverrides(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   map[int]int
	}{
		"explicit replaces inherited rail": {
			source: "planes: &planes\n  0: 0\n  1: 1\nclusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      <<: *planes\n      0: 2\n",
			want:   map[int]int{0: 2, 1: 1},
		},
		"explicit numeric alias replaces inherited rail": {
			source: "planes: &planes\n  0x0: 0\nclusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      <<: *planes\n      0: 2\n",
			want:   map[int]int{0: 2},
		},
		"earlier merge mapping wins": {
			source: "first: &first\n  0: 1\nsecond: &second\n  0: 2\n  1: 3\nclusterConfig:\n- spectrumX:\n    swPlaneByRail:\n      <<: [*first, *second]\n",
			want:   map[int]int{0: 1, 1: 3},
		},
		"explicit spectrum-x replaces inherited block": {
			source: "group: &group\n  spectrumX:\n    swPlaneByRail:\n      0: 1\nclusterConfig:\n- <<: *group\n  spectrumX:\n    swPlaneByRail:\n      0: 2\n",
			want:   map[int]int{0: 2},
		},
		"aliased spectrum-x block": {
			source: "sx: &sx\n  swPlaneByRail:\n    0: 1\nclusterConfig:\n- spectrumX: *sx\n",
			want:   map[int]int{0: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			input, err := DecodeInput([]byte(tc.source), "planes.yaml")
			require.NoError(t, err)
			require.Equal(t, tc.want, input.Config.ClusterConfig[0].SpectrumX.SwPlaneByRail)
		})
	}
}
