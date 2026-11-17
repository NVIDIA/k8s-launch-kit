// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunShortTrafficBudgetStopsFreshAndResumeBeforeAnyTransport(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, resume := range []bool{false, true} {
		request := imagePrerequisiteRunRequest(t, execution.retained, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
			t.Fatal("prerequisites/probes/effects ran despite impossible budget")
			return PrerequisiteVerification{}, nil
		})
		if resume {
			request.Resume = &OperationRef{Namespace: "dpf-operator-system", Name: "dpf-op-original", UID: types.UID("123e4567-e89b-12d3-a456-426614174006")}
		}
		original := request.Resume
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		result, err := runWithTransports(ctx, request, runTransports{resolveImage: func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
			t.Fatal("image metadata transport ran despite impossible budget")
			return hbnPodImageIdentity{}, nil
		}})
		cancel()
		if request.Resume != original {
			t.Fatal("Run changed caller resume input")
		}
		if err == nil || !strings.Contains(err.Error(), "HBN_RUNTIME_BUDGET_INSUFFICIENT") || result.State != RunStateFailed || result.LastCompletedStage != "" || result.Operation != nil || result.Activation != nil || len(result.HostNetworking) != 0 || result.HBNRuntime != nil {
			t.Fatalf("budget failure escaped early boundary: result=%+v err=%v", result, err)
		}
		report, readErr := os.ReadFile(request.ReportPath)
		if readErr != nil || !strings.Contains(string(report), "HBN_RUNTIME_BUDGET_INSUFFICIENT") || strings.Contains(string(report), "HBN_IMAGE_METADATA_STARTED") || strings.Contains(string(report), "FRESH_PREREQUISITES_STARTED") {
			t.Fatalf("missing/late budget report: %s %v", report, readErr)
		}
	}
}

func TestActivationRechecksBudgetBeforeFirstDeploymentCreation(t *testing.T) {
	f := newActivationIntegrationFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	_, err := runActivationStage(ctx, f.client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
		RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
			t.Fatal("reset after insufficient remaining matrix budget")
			return ActivationObservation{}, nil
		}))
	if err == nil || !strings.Contains(err.Error(), "HBN_RUNTIME_BUDGET_INSUFFICIENT") || f.client.deployment != nil || f.client.guardCalls != 0 || len(f.client.patches) != 0 {
		t.Fatalf("late budget check allowed physical root/effects: err=%v deployment=%v claims=%d patches=%v", err, f.client.deployment, f.client.guardCalls, f.client.patches)
	}
}
