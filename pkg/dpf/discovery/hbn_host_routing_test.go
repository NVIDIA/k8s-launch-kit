// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestHostManagementProtectionIncludesNonMainRouteTables(t *testing.T) {
	inv := hostHBNTestInventory()
	inv.Hosts[0].Runtime.Routes = []inventory.Route{{
		Destination: "0.0.0.0/0", Gateway: "192.0.2.1", InterfaceIndex: 10, Table: 1001,
	}}
	checks := hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "failed" || got.ReasonCode != "HOST_IPV4_DEFAULT_ROUTE_ON_SELECTED_PATH" {
		t.Fatalf("default route in non-main table was ignored: %#v", got)
	}
}
