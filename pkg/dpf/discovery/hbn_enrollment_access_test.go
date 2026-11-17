// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestHBNEnrollmentCompatibilityOwnsSafeCheckAndReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason, wantState, wantReason string
	}{
		{"compatible", "passed", "ENROLLMENT_CREDENTIALS_COMPATIBLE", "passed", "ENROLLMENT_CREDENTIALS_COMPATIBLE"},
		{"incompatible", "failed", "ENROLLMENT_CREDENTIALS_INCOMPATIBLE", "failed", "ENROLLMENT_CREDENTIALS_INCOMPATIBLE"},
		{"identity unavailable", "unknown", "ENROLLMENT_IDENTITY_UNAVAILABLE", "unknown", "ENROLLMENT_IDENTITY_UNAVAILABLE"},
		{"false pass", "passed", "ENROLLMENT_CREDENTIALS_INCOMPATIBLE", "unknown", "ENROLLMENT_CREDENTIALS_RESULT_INVALID"},
		{"arbitrary reason", "passed", "private-password-hash", "unknown", "ENROLLMENT_CREDENTIALS_RESULT_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := hbn.Resolve(hbn.Options{})
			if err != nil {
				t.Fatal(err)
			}
			request := config.Request{HBN: &profile}
			current := authorizationTestInventory("https://current.example.invalid", "current-uid")
			var progress strings.Builder
			ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
			calls := 0
			validator := func(got config.Request, inv *inventory.Inventory) inventory.Check {
				calls++
				if got.HBN != request.HBN || inv != current {
					t.Fatal("validator lost current request/inventory binding")
				}
				return inventory.Check{ID: hbnEnrollmentAccessID, State: tc.state, ReasonCode: tc.reason,
					Vantage: "executor", RequiredPhase: "deploy", Subject: "credential-canary", Message: "credential-canary"}
			}
			report := &inventory.Report{Inventory: current, Discovery: inventory.Discovery{Outcome: "complete", Checks: []inventory.Check{}}}
			collectHBN(ctx, request, report, nil, Dependencies{HBNEnrollment: validator})
			check := readinessCheck(t, report.Readiness.Checks, hbnEnrollmentAccessID)
			if calls != 1 || check.State != tc.wantState || check.ReasonCode != tc.wantReason {
				t.Fatalf("unexpected callback/check: calls=%d check=%+v", calls, check)
			}
			if report.Discovery.Outcome != "complete" {
				t.Fatal("HBN credential compatibility changed general inventory outcome")
			}
			if check.State != "passed" && report.Readiness.State != "blocked" {
				t.Fatal("missing credential compatibility did not block HBN readiness")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded)+progress.String(), "credential-canary") || strings.Contains(string(encoded)+progress.String(), "private-password-hash") {
				t.Fatal("callback private text escaped into report/progress")
			}
		})
	}
}

func TestHBNEnrollmentCompatibilityRejectsWrongCheckScope(t *testing.T) {
	for _, edit := range []func(*inventory.Check){
		func(c *inventory.Check) { c.ID = "dpf.hbn.management.caller-rbac.secret" },
		func(c *inventory.Check) { c.RequiredPhase = "discover" },
		func(c *inventory.Check) { c.Vantage = "remote" },
	} {
		result := inventory.Check{ID: hbnEnrollmentAccessID, Vantage: "executor", RequiredPhase: "deploy", State: "passed", ReasonCode: "ENROLLMENT_CREDENTIALS_COMPATIBLE"}
		edit(&result)
		check := hbnEnrollmentAccessCheck(context.Background(), config.Request{}, nil, func(config.Request, *inventory.Inventory) inventory.Check { return result })
		if check.State != "unknown" || check.ReasonCode != "ENROLLMENT_CREDENTIALS_RESULT_INVALID" {
			t.Fatalf("unsupported scope accepted: %+v", check)
		}
	}
}

func TestHBNEnrollmentCompatibilityUnavailableCancelledAndGenericInventory(t *testing.T) {
	if check := hbnEnrollmentAccessCheck(context.Background(), config.Request{}, nil, nil); check.State != "unknown" || check.ReasonCode != "ENROLLMENT_CREDENTIALS_VALIDATOR_UNAVAILABLE" {
		t.Fatalf("missing validator accepted: %+v", check)
	}
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	validate := func(config.Request, *inventory.Inventory) inventory.Check {
		calls++
		cancel()
		return inventory.Check{ID: hbnEnrollmentAccessID, Vantage: "executor", RequiredPhase: "deploy", State: "passed", ReasonCode: "ENROLLMENT_CREDENTIALS_COMPATIBLE"}
	}
	if check := hbnEnrollmentAccessCheck(ctx, config.Request{}, nil, validate); check.State != "unknown" || check.ReasonCode != "ENROLLMENT_CREDENTIALS_CHECK_CANCELLED" || calls != 1 {
		t.Fatalf("late result accepted: calls=%d check=%+v", calls, check)
	}
	if check := hbnEnrollmentAccessCheck(ctx, config.Request{}, nil, validate); check.ReasonCode != "ENROLLMENT_CREDENTIALS_CHECK_CANCELLED" || calls != 1 {
		t.Fatal("cancelled validator invoked")
	}
	report := &inventory.Report{Inventory: &inventory.Inventory{}, Discovery: inventory.Discovery{Outcome: "complete"}}
	collectHBN(context.Background(), config.Request{}, report, nil, Dependencies{HBNEnrollment: validate})
	if calls != 1 || report.Readiness != nil || report.Discovery.Outcome != "complete" {
		t.Fatal("general inventory invoked HBN crawler policy")
	}
}
