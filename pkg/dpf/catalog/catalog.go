// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package catalog owns qualified discovery assets. Site inputs cannot waive these gates.
package catalog

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
)

// ProbeAssets pins the existing netshoot v0.16 image. Registry and local non-root
// tool checks do not qualify Kubernetes admission, so activation remains gated.
// No discovery image is built or published by Launch Kit.
func ProbeAssets() probe.Assets {
	return probe.Assets{
		Repository:     "docker.io/nicolaka/netshoot",
		ManifestDigest: "sha256:b09d9b21381f47a79b3cbcb30da25266dc17186ea00ae65e99fdc51396f48e70",
		PlatformDigests: map[string]string{
			"amd64": "sha256:147f2c4cb0aa7a741151bce2bfef71d9e1a04631636e24665ee2a5ed799feb47",
			"arm64": "sha256:f460dc5e85890cad42b14973f5724b40831cbb3e1f745470caf6b8cf0d6ec7fe",
		},
		Qualified: false,
	}
}

// Hardware is empty until real resource graphs and adapter completeness predicates
// have been qualified for specific models/firmware. Candidate reads remain useful.
func Hardware() []redfish.Qualification { return nil }
