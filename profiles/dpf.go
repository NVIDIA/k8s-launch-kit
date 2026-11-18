// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package profiles embeds DPF-specific profile assets into the standalone
// Launch Kit binary.
package profiles

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed dpf-hbn-bgp/profile.yaml
var dpfHBNBGPProfile []byte

//go:embed dpf-hbn-bgp/flavors/*.yaml
var dpfHBNBGPFlavors embed.FS

// DPFHBNBGPProfile returns an independent copy of the embedded DPF HBN profile.
func DPFHBNBGPProfile() []byte {
	return append([]byte(nil), dpfHBNBGPProfile...)
}

// DPFHBNBGPFlavor returns a copy of a maintained HBN DPUFlavor spec asset.
func DPFHBNBGPFlavor(name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid HBN flavor asset name")
	}
	path := "dpf-hbn-bgp/flavors/" + name + ".yaml"
	if !fs.ValidPath(path) {
		return nil, fmt.Errorf("invalid HBN flavor asset path")
	}
	data, err := dpfHBNBGPFlavors.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("HBN flavor asset %q is unavailable", name)
	}
	return data, nil
}
