// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF deployment CLI results", func() {
	It("emits one JSON result on failure and leaves exit handling to the command", func() {
		var stdout, stderr bytes.Buffer
		result := deployment.RunResult{State: deployment.RunStateFailed, LastCompletedStage: "enrollment", ReportPath: "/reports/deployment.jsonl"}
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", result, errors.New("private diagnostic detail"))
		Expect(apperrors.IsReported(err)).To(BeTrue())
		Expect(apperrors.ExitCodeFromError(err)).To(Equal(apperrors.ExitDeployment))
		Expect(stdout.String()).NotTo(ContainSubstring("private diagnostic detail"))
		decoder := json.NewDecoder(&stdout)
		var outcome map[string]any
		Expect(decoder.Decode(&outcome)).To(Succeed())
		Expect(outcome["state"]).To(Equal("failed"))
		Expect(outcome["lastCompletedStage"]).To(Equal("enrollment"))
		Expect(outcome["reportPath"]).To(Equal("/reports/deployment.jsonl"))
		Expect(outcome["error"]).To(HaveKeyWithValue("category", "deployment"))
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

	It("reports an early failure without advertising a missing report", func() {
		var stdout, stderr bytes.Buffer
		err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, "json", deployment.RunResult{}, errors.New("cannot create journal"))
		Expect(apperrors.IsReported(err)).To(BeTrue())
		var outcome map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &outcome)).To(Succeed())
		Expect(outcome["state"]).To(Equal("failed"))
		Expect(outcome["reportPath"]).To(BeEmpty())
		Expect(outcome["error"]).To(HaveKeyWithValue("message", "DPF deployment stopped before a report could be created"))
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
