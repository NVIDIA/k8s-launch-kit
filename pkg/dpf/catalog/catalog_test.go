// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package catalog

import "testing"

func TestProbeAssetsPinOnlyLinuxAMD64(t *testing.T) {
	assets := ProbeAssets()
	if err := assets.Validate(); err != nil {
		t.Fatalf("probe assets are not enabled for candidate runtime qualification: %v", err)
	}
	if assets.Repository != "docker.io/nicolaka/netshoot" || assets.ManifestDigest == "" {
		t.Fatalf("probe image is not pinned: %+v", assets)
	}
	if len(assets.PlatformDigests) != 1 || assets.PlatformDigests["amd64"] == "" {
		t.Fatalf("only runtime-qualified Linux/amd64 should be enabled: %+v", assets.PlatformDigests)
	}
}

func TestCumulusASICReaderIdentitiesAreIndependentValues(t *testing.T) {
	expected := CumulusASICReaderProfile{
		OSID:         "cumulus-linux",
		Version:      "5.15.1",
		CodeDrop:     "CD28 Base 5.deb12.dev.1481 - SDK 4.8.2052 - Nvue Core Tag 1.11.0.79 - Nvue OM Tag 1.3.33.61",
		RIFSHA256:    "e58025b5dba285a4041099bf4bad6a3b6c81b9c34cb8aa42d43b5cc08456f608",
		HelperSHA256: "2f0424de6dfc867056f2852e9af937334b2cd256cf11a9dcaa007d940030e9cf",
	}
	first := CumulusASICReader()
	if first != expected {
		t.Fatalf("unexpected qualified reader identities: %+v", first)
	}
	first.OSID = "other-linux"
	first.Version = "unreviewed"
	first.CodeDrop = "unreviewed"
	first.RIFSHA256 = "unreviewed"
	first.HelperSHA256 = "unreviewed"
	if next := CumulusASICReader(); next != expected || next == first {
		t.Fatalf("caller changed shared qualification: %+v", next)
	}
}
