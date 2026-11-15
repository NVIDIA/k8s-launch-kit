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
