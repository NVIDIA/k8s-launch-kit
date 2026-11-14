// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package profiles embeds DPF-specific profile assets into the standalone
// Launch Kit binary.
package profiles

import (
	_ "embed"
)

//go:embed dpf-hbn-bgp/profile.yaml
var dpfHBNBGPProfile []byte

// DPFHBNBGPProfile returns an independent copy of the embedded DPF HBN profile.
func DPFHBNBGPProfile() []byte {
	return append([]byte(nil), dpfHBNBGPProfile...)
}
