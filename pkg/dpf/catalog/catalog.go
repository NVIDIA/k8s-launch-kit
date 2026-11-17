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

// CumulusASICReaderProfile identifies the one reviewed stock diagnostic and
// switchd/NVUE format contract. It is independent of the selected DPF release.
// Fields are values so callers cannot change the qualification for later reads.
type CumulusASICReaderProfile struct {
	OSID         string
	Version      string
	CodeDrop     string
	RIFSHA256    string
	HelperSHA256 string
}

// CumulusASICReader returns the supported reader identities. Site inputs cannot
// waive these checks; another build needs independent source/runtime qualification.
// This profile qualifies the reader, not every hardware/topology combination.
func CumulusASICReader() CumulusASICReaderProfile {
	return CumulusASICReaderProfile{
		OSID:         "cumulus-linux",
		Version:      "5.15.1",
		CodeDrop:     "CD28 Base 5.deb12.dev.1481 - SDK 4.8.2052 - Nvue Core Tag 1.11.0.79 - Nvue OM Tag 1.3.33.61",
		RIFSHA256:    "e58025b5dba285a4041099bf4bad6a3b6c81b9c34cb8aa42d43b5cc08456f608",
		HelperSHA256: "2f0424de6dfc867056f2852e9af937334b2cd256cf11a9dcaa007d940030e9cf",
	}
}
