// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

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
				original := testTenant(f.controlPlane.tenant.Client).source
				injected := false
				closing := phase == "entry"
				testTenant(f.controlPlane.tenant.Client).source = tenantWaitSourceFunc(func(ctx context.Context, expected controllercontract.TenantReadinessExpected) (tenantSources, error) {
					if closing {
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
							testTenant(f.controlPlane.tenant.Client).systemUID = "different-current-uid"
						}
					}
					return original.observeTenantSources(ctx, expected)
				})
				patches, claims := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls
				fences := fixtureAcceptanceFences(t, f, f.client, journal)
				if phase == "exit" {
					if err := fences.physical(ctx); err != nil {
						t.Fatal(err)
					}
					closing = true
				}
				err := fences.physical(ctx)
				want := "TENANT_API_IDENTITY_CHANGED"
				switch scenario {
				case "cancel", "deadline":
					want = "ACTIVATION_CANCELED"
				case "unavailable":
					want = "TENANT_API_IDENTITY_UNVERIFIED"
				}
				if !injected || err == nil || !strings.Contains(file.String(), `"reasonCode":"`+want+`"`) {
					t.Fatalf("injected=%v err=%v journal=%s", injected, err, file.String())
				}
				if scenario == "cancel" && !errors.Is(err, context.Canceled) || scenario == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("runtime boundary lost context cause")
				}
				encoded, marshalErr := json.Marshal(managementIdentityDiagnostic(ctx, err))
				if marshalErr != nil || strings.Contains(file.String()+err.Error()+string(encoded), "private-token-api-canary") || strings.Contains(file.String(), "HBN_RUNTIME_COHORT_VALIDATED") {
					t.Fatal("raw failure leaked or cohort acceptance retained")
				}
				if f.commandReads != 0 || f.trafficReads != 0 {
					t.Fatal("receipt boundary measured runtime")
				}
				if len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims {
					t.Fatal("runtime introduced a mutation or reboot claim")
				}
			})
		}
	}
}
