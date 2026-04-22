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

package pciids

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookupNVIDIA_KnownDevices(t *testing.T) {
	// Device IDs picked from the embedded vendor 10de block. These assertions
	// double as a regression guard on the canonicalization rules: bracketed
	// marketing names win over chip codenames, spaces become dashes, and a
	// "NVIDIA-" prefix is ensured.
	tests := []struct {
		name     string
		deviceID string
		want     string
	}{
		{"H100 SXM5 80GB", "2330", "NVIDIA-H100-SXM5-80GB"},
		{"H200 SXM 141GB", "2335", "NVIDIA-H200-SXM-141GB"},
		{"H200 NVL", "233b", "NVIDIA-H200-NVL"},
		{"L40", "26b5", "NVIDIA-L40"},
		{"L40S", "26b9", "NVIDIA-L40S"},
		{"B200", "2901", "NVIDIA-B200"},
		{"with 0x prefix", "0x2335", "NVIDIA-H200-SXM-141GB"},
		{"uppercase", "0X233B", "NVIDIA-H200-NVL"},
		{"trailing whitespace", "  2335\n", "NVIDIA-H200-SXM-141GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LookupNVIDIA(tt.deviceID))
		})
	}
}

func TestLookupNVIDIA_Unknown(t *testing.T) {
	tests := []struct {
		name     string
		deviceID string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"non-hex", "zzzz"},
		{"not in table", "ffff"},
		{"prefix only", "0x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, "", LookupNVIDIA(tt.deviceID))
		})
	}
}

func TestParseVendorBlock(t *testing.T) {
	input := "10de  NVIDIA Corporation\n" +
		"\t1111  ChipA [Product A]\n" +
		"\t\t1043 0001  Subsystem ignored\n" +
		"\t2222  Product B\n" +
		"\t\t1043 0002  Subsystem ignored\n" +
		"\t3333  \n" + // blank name -> skipped
		"# comment\n" +
		"bogus-line\n"
	got := parseVendorBlock(input)
	assert.Equal(t, "ChipA [Product A]", got["1111"])
	assert.Equal(t, "Product B", got["2222"])
	assert.NotContains(t, got, "3333")
	assert.Len(t, got, 2)
}

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"bracketed wins", "GH100 [H200 SXM 141GB]", "NVIDIA-H200-SXM-141GB"},
		{"no brackets", "H100 NVL", "NVIDIA-H100-NVL"},
		{"already NVIDIA-prefixed", "NVIDIA L40S", "NVIDIA-L40S"},
		{"lowercase nvidia", "nvidia GeForce RTX 4090", "nvidia-GeForce-RTX-4090"},
		{"empty brackets fall back", "Chip []", "NVIDIA-Chip-[]"},
		{"whitespace trimmed", "  L40  ", "NVIDIA-L40"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, canonicalize(tt.raw))
		})
	}
}

func TestEmbedLoaded(t *testing.T) {
	// Guard against the embed going empty (e.g. accidentally deleted nvidia.ids).
	assert.Greater(t, len(deviceNames), 100, "embedded NVIDIA device table should have many entries")
}
