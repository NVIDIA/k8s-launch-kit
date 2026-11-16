// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFinalPreflightIdentityReadFailureRevokesPassedChecksTruthfully(t *testing.T) {
	for _, owner := range []string{"permissions", "prerequisites"} {
		for _, scenario := range []string{"cancel", "unavailable", "UID drift"} {
			t.Run(owner+"/"+scenario, func(t *testing.T) {
				harness := newCallerPermissionHarness(t, nil)
				generated, err := generateFixtureBundle(callerGenerationInputs(t, harness.server))
				if err != nil {
					t.Fatal(err)
				}
				plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: generated})
				if err != nil {
					t.Fatal(err)
				}
				harness.projection = plan.projection
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				hit := false
				harness.identityReadHook = func(w http.ResponseWriter, read int) bool {
					if read != 2 {
						return false
					}
					hit = true
					switch scenario {
					case "cancel":
						cancel()
						return true
					case "unavailable":
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","reason":"ServiceUnavailable","code":503,"message":"private-preflight-canary"}`))
						return true
					default:
						return false
					}
				}
				if scenario == "UID drift" {
					harness.identityUIDChangeAtGet = 2
				}
				client := callerPermissionClient(t, harness.server)
				var result PreflightResult
				if owner == "permissions" {
					result = client.checkCallerPermissions(ctx, plan)
				} else {
					result = client.checkManagementPreflight(ctx, plan)
				}
				want := "MANAGEMENT_IDENTITY_CHANGED"
				switch scenario {
				case "cancel":
					want = "MANAGEMENT_IDENTITY_CANCELED"
				case "unavailable":
					want = "MANAGEMENT_IDENTITY_UNVERIFIED"
				}
				found := false
				for _, check := range result.Checks {
					if check.ID == "management.identity-final" && check.ReasonCode == want {
						found = true
					}
					if check.State == "passed" {
						t.Fatalf("stale passed check survived: %+v", check)
					}
				}
				data, _ := json.Marshal(result)
				if !hit || result.Ready || !found || strings.Contains(string(data), "private-preflight-canary") {
					t.Fatalf("hit=%v result=%s", hit, data)
				}
			})
		}
	}
}
