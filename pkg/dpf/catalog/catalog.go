// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package catalog owns qualified discovery assets. Site inputs cannot waive these gates.
package catalog

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

// ProbeAssets pins the existing netshoot v0.16 image. The installed protocol is
// supported on Linux/amd64 in pod and host networking; other architectures
// remain unavailable.
// No discovery image is built or published by Launch Kit.
func ProbeAssets() probe.Assets {
	return probe.Assets{
		Repository:     "docker.io/nicolaka/netshoot",
		ManifestDigest: "sha256:b09d9b21381f47a79b3cbcb30da25266dc17186ea00ae65e99fdc51396f48e70",
		PlatformDigests: map[string]string{
			"amd64": "sha256:147f2c4cb0aa7a741151bce2bfef71d9e1a04631636e24665ee2a5ed799feb47",
		},
		Qualified: true,
	}
}
