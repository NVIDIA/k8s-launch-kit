// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
)

func TestBootstrapConsumesOnlyTheExplicitControllerServiceCIDR(t *testing.T) {
	vip, iface := "192.0.2.20", "eth0"
	options := &BootstrapOptions{Tenant: TenantOptions{VIP: &vip, Interface: &iface}}
	selected := []bootstrap.Device{{Serial: "DPU-A", BMCIP: "192.0.2.30", BMCPort: 443}}
	before, _ := json.Marshal([]any{options, selected})
	for _, value := range []string{"172.31.0.0/16", "", "bad", "172.31.1.0/16", "2001:db8::/64", "192.0.2.0/24", "10.244.0.0/14"} {
		got, issues, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected, value)
		if value == "172.31.0.0/16" {
			if err != nil || len(issues) != 0 || got.Tenant.ServiceCIDR != value {
				t.Fatalf("explicit controller scalar lost: %+v %+v %v", got, issues, err)
			}
		} else if err == nil {
			t.Fatalf("invalid/missing/conflicting controller scalar accepted: %q", value)
		}
	}
	after, _ := json.Marshal([]any{options, selected})
	if !bytes.Equal(before, after) {
		t.Fatal("bootstrap resolution mutated user/selected input")
	}
}
