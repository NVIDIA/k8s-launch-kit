// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementnetwork

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
)

func TestCheckTenantAllocationPreservesOptionalAndExplicitValues(t *testing.T) {
	trueValue, falseValue := true, false
	tests := []struct {
		name       string
		allocation *bootstrap.SiteAllocation
		states     []string
		reasons    []string
	}{
		{
			name:    "omitted",
			states:  []string{"unknown", "unknown", "unknown"},
			reasons: []string{"TENANT_SITE_ALLOCATION_MISSING", "TENANT_SITE_ALLOCATION_MISSING", "TENANT_SITE_ALLOCATION_MISSING"},
		},
		{
			name:       "partial",
			allocation: &bootstrap.SiteAllocation{VIPReserved: &trueValue},
			states:     []string{"passed", "unknown", "unknown"},
			reasons:    []string{"TENANT_SITE_ALLOCATION_DECLARED", "TENANT_SITE_ALLOCATION_MISSING", "TENANT_SITE_ALLOCATION_MISSING"},
		},
		{
			name:       "explicit rejection",
			allocation: &bootstrap.SiteAllocation{VIPReserved: &trueValue, VIPExcludedFromDHCP: &falseValue, RouterIDUniqueOnSelectedNodes: &trueValue},
			states:     []string{"passed", "failed", "passed"},
			reasons:    []string{"TENANT_SITE_ALLOCATION_DECLARED", "TENANT_SITE_ALLOCATION_REJECTED", "TENANT_SITE_ALLOCATION_DECLARED"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := CheckTenantAllocation(bootstrap.Tenant{Allocation: test.allocation})
			if len(checks) != 3 {
				t.Fatalf("got %d allocation checks, want 3: %#v", len(checks), checks)
			}
			for i, check := range checks {
				if check.State != test.states[i] || check.ReasonCode != test.reasons[i] || check.RequiredPhase != "deploy" || check.Vantage != "site-config" {
					t.Fatalf("check[%d] = %#v", i, check)
				}
				if !strings.HasPrefix(check.Subject, "/provisioning/bootstrap/tenant/allocation/") {
					t.Fatalf("check subject lost exact field path: %#v", check)
				}
			}
		})
	}
}

func TestCheckTenantAllocationAllTrueIsReportedAsDeclared(t *testing.T) {
	reserved, excluded, unique := true, true, true
	checks := CheckTenantAllocation(bootstrap.Tenant{Allocation: &bootstrap.SiteAllocation{
		VIPReserved: &reserved, VIPExcludedFromDHCP: &excluded, RouterIDUniqueOnSelectedNodes: &unique,
	}})
	for _, check := range checks {
		if check.State != "passed" || check.ReasonCode != "TENANT_SITE_ALLOCATION_DECLARED" || !strings.Contains(check.Message, "declares") || !strings.Contains(check.Message, "did not independently observe") {
			t.Fatalf("true declaration overstated evidence: %#v", check)
		}
	}
}
