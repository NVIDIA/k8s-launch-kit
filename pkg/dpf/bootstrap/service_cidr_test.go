// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import "testing"

func TestBootstrapCIDRValidationIsNotPinnedToOneRelease(t *testing.T) {
	in := inputs()
	in.Tenant.ServiceCIDR = "172.31.0.0/16"
	if err := Validate(in); err != nil {
		t.Fatalf("generic bootstrap validation rejected a valid selected-controller CIDR: %v", err)
	}
}
