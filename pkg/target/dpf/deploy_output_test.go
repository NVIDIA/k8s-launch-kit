// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF deployment CLI results", func() {
	It("retains a safe reservation reason in JSON checks without exposing its private cause", func() {
		var stdout, stderr bytes.Buffer
		check := inventory.Check{ID: "dpf.hbn.deployment.operation-reservation", Subject: "operator-system/dpf-op-example", Vantage: "management-cluster", RequiredPhase: "deploy",
			State: "failed", ReasonCode: "OPERATION_RESERVATION_EXISTS", Message: "Preserve the existing record and inspect its original deployment before recovery or retirement."}
		result := deployment.RunResult{State: deployment.RunStateFailed, ReportPath: "/reports/deployment.jsonl", Checks: []inventory.Check{check}}
		cause := errors.New("private-kubeconfig-token-canary")
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", result, cause)
		Expect(apperrors.IsReported(err)).To(BeTrue())
		Expect(errors.Is(err, cause)).To(BeTrue())
		Expect(stdout.String()).NotTo(ContainSubstring(cause.Error()))
		decoder := json.NewDecoder(&stdout)
		var outcome struct {
			Checks []inventory.Check `json:"checks"`
			Error  map[string]any    `json:"error"`
		}
		Expect(decoder.Decode(&outcome)).To(Succeed())
		Expect(outcome.Checks).To(Equal([]inventory.Check{check}))
		Expect(outcome.Error).To(HaveKeyWithValue("transient", false))
		Expect(decoder.Decode(&outcome)).To(MatchError(io.EOF))
		Expect(stderr.String()).To(BeEmpty())
	})

	It("emits one JSON result on failure and leaves exit handling to the command", func() {
		var stdout, stderr bytes.Buffer
		result := deployment.RunResult{
			State: deployment.RunStateFailed, LastCompletedStage: "enrollment", ReportPath: "/reports/deployment.jsonl",
			Operation: &deployment.OperationRef{Namespace: "operator-system", Name: "dpf-op-example", UID: "123e4567-e89b-12d3-a456-426614174000"},
		}
		cause := errors.New("private diagnostic detail")
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", result, cause)
		Expect(apperrors.IsReported(err)).To(BeTrue())
		Expect(apperrors.ExitCodeFromError(err)).To(Equal(apperrors.ExitDeployment))
		var structured *apperrors.StructuredError
		Expect(errors.As(err, &structured)).To(BeTrue())
		Expect(structured.Transient).To(BeFalse())
		Expect(errors.Is(err, cause)).To(BeTrue())
		Expect(stdout.String()).NotTo(ContainSubstring("private diagnostic detail"))
		decoder := json.NewDecoder(&stdout)
		var outcome map[string]any
		Expect(decoder.Decode(&outcome)).To(Succeed())
		Expect(outcome["state"]).To(Equal("failed"))
		Expect(outcome["lastCompletedStage"]).To(Equal("enrollment"))
		Expect(outcome["reportPath"]).To(Equal("/reports/deployment.jsonl"))
		Expect(outcome["error"]).To(HaveKeyWithValue("category", "deployment"))
		Expect(outcome["error"]).To(HaveKeyWithValue("transient", false))
		Expect(outcome["operation"]).To(HaveKeyWithValue("name", "dpf-op-example"))
		Expect(decoder.Decode(&outcome)).To(MatchError(io.EOF))
		Expect(stderr.String()).To(BeEmpty())
	})

	It("returns the complete success result without an error object", func() {
		var stdout, stderr bytes.Buffer
		result := deployment.RunResult{State: deployment.RunStateValidated, LastCompletedStage: "runtime", ReportPath: "/reports/deployment.jsonl", TrafficValidated: true}
		Expect(writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", result, nil)).To(Succeed())
		var outcome map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &outcome)).To(Succeed())
		Expect(outcome).NotTo(HaveKey("error"))
		Expect(outcome["trafficValidated"]).To(BeTrue())
	})

	It("reports an early failure without advertising a missing report or retrying automatically", func() {
		var stdout, stderr bytes.Buffer
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", deployment.RunResult{}, errors.New("cannot create journal"))
		Expect(apperrors.IsReported(err)).To(BeTrue())
		var structured *apperrors.StructuredError
		Expect(errors.As(err, &structured)).To(BeTrue())
		Expect(structured.Transient).To(BeFalse())
		var outcome map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &outcome)).To(Succeed())
		Expect(outcome["state"]).To(Equal("failed"))
		Expect(outcome["reportPath"]).To(BeEmpty())
		Expect(outcome["error"]).To(HaveKeyWithValue("message", "DPF deployment stopped before a report could be created"))
		Expect(outcome["error"]).To(HaveKeyWithValue("transient", false))
	})

	It("marks interrupted standalone deployment results action-required", func() {
		var stdout, stderr bytes.Buffer
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", deployment.RunResult{State: deployment.RunStateFailed, ReportPath: "/reports/deployment.jsonl"}, context.Canceled)
		var structured *apperrors.StructuredError
		Expect(errors.As(err, &structured)).To(BeTrue())
		Expect(structured.Transient).To(BeFalse())
		var outcome map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &outcome)).To(Succeed())
		Expect(outcome["error"]).To(HaveKeyWithValue("transient", false))
		Expect(stdout.String()).NotTo(ContainSubstring("context canceled"))
	})

	It("fails when the final JSON result cannot be written", func() {
		var stderr bytes.Buffer
		err := writeDeploymentResult(DeployRequest{Stdout: failedWriter{}, Stderr: &stderr}, "json", deployment.RunResult{State: deployment.RunStateValidated}, nil)
		Expect(err).To(HaveOccurred())
		Expect(apperrors.IsReported(err)).To(BeTrue())
		Expect(stderr.String()).To(ContainSubstring("cannot write complete DPF deployment result"))
	})

	It("preserves text failure diagnostics for the outer command", func() {
		var stdout, stderr bytes.Buffer
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "text", deployment.RunResult{State: deployment.RunStateFailed}, errors.New("device reset not confirmed"))
		Expect(err).To(MatchError(ContainSubstring("device reset not confirmed")))
		Expect(apperrors.IsReported(err)).To(BeFalse())
		Expect(stdout.String()).To(ContainSubstring("DPF deployment: failed"))
	})

	It("rejects unsupported modes and invalid deadlines before opening access", func() {
		adapter := NewDeployAdapter(DeployRequest{UserConfig: "/site.yaml", DeploymentFiles: "/bundle"})
		for _, invocation := range []target.Invocation{
			{Target: target.DPF, Phase: target.Deploy, Execution: target.ExecutionOptions{DryRun: true}},
			{Target: target.DPF, Phase: target.Deploy, Execution: target.ExecutionOptions{Timeout: -time.Second}},
			{Target: target.DPF, Phase: target.Deploy, Execution: target.ExecutionOptions{Timeout: 3 * time.Hour}},
			{Target: target.DPF, Phase: target.Validate},
		} {
			operation, err := adapter.Bind(invocation)
			Expect(err).To(HaveOccurred())
			Expect(operation).To(BeNil())
		}
	})
})
