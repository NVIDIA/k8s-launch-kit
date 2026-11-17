// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestHBNRuntimeIdentityFailureKeepsTruthfulReasonAndCause(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, phase := range []string{"entry", "exit"} {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "UID drift"} {
			t.Run(phase+"/"+scenario, func(t *testing.T) {
				f := newRuntimeStageFixtureFromExecution(t, execution)
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				f.ctx = ctx
				journal := testJournal(execution)
				file := journal.file.(*managementMemoryFile)
				original := f.controlPlane.tenant.Client.source
				injected := false
				f.controlPlane.tenant.Client.source = tenantWaitSourceFunc(func(ctx context.Context, expected TenantReadinessExpected) (tenantSources, error) {
					ready := phase == "entry"
					if phase == "exit" {
						ready = runtimeDiagnosticExitReady(t, file)
					}
					if ready {
						injected = true
						switch scenario {
						case "cancel":
							cancel()
							return tenantSources{}, ctx.Err()
						case "deadline":
							return tenantSources{}, context.DeadlineExceeded
						case "unavailable":
							return tenantSources{}, apierrors.NewServiceUnavailable("private-token-api-canary")
						case "UID drift":
							f.controlPlane.tenant.Client.systemUID = "different-current-uid"
						}
					}
					return original.observeTenantSources(ctx, expected)
				})
				patches, claims := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls
				result, err := f.run(journal)
				want, wantRuntime := "TENANT_API_IDENTITY_CHANGED", "HBN_RUNTIME_PHYSICAL_ENTRY_FAILED"
				if phase == "exit" {
					wantRuntime = "HBN_RUNTIME_PHYSICAL_EXIT_FAILED"
				}
				switch scenario {
				case "cancel", "deadline":
					want, wantRuntime = "ACTIVATION_CANCELED", "HBN_RUNTIME_CANCELED"
				case "unavailable":
					want = "TENANT_API_IDENTITY_UNVERIFIED"
				}
				if !injected || err == nil || result.State != "failed" || result.TrafficValidated || result.ReasonCode != wantRuntime || !strings.Contains(file.String(), `"reasonCode":"`+want+`"`) {
					t.Fatalf("injected=%v result=%+v err=%v journal=%s", injected, result, err, file.String())
				}
				if scenario == "cancel" && !errors.Is(err, context.Canceled) || scenario == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("runtime boundary lost context cause")
				}
				encoded, marshalErr := json.Marshal(result)
				if marshalErr != nil || strings.Contains(file.String()+err.Error()+string(encoded), "private-token-api-canary") || strings.Contains(file.String(), "HBN_RUNTIME_COHORT_VALIDATED") {
					t.Fatal("raw failure leaked or cohort acceptance retained")
				}
				if phase == "entry" && (f.commandReads != 0 || f.trafficReads != 0 || f.leaseReads != 0) || phase == "exit" && (len(result.BGP) != 2 || len(result.Routes) != 2 || len(result.Traffic) != 2) {
					t.Fatal("failure injection did not reach its intended boundary")
				}
				if len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims {
					t.Fatal("runtime introduced a mutation or reboot claim")
				}
			})
		}
	}
}

// The final confirmed traffic event is after its post-measurement fence and
// immediately before the full physical exit check. No API call count is assumed.
func runtimeDiagnosticExitReady(t *testing.T, file *managementMemoryFile) bool {
	t.Helper()
	confirmed := 0
	for _, line := range strings.Split(strings.TrimSpace(file.String()), "\n") {
		if line == "" {
			continue
		}
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Operation == "traffic" && event.State == "confirmed" {
			confirmed++
		}
	}
	return confirmed == 2
}
