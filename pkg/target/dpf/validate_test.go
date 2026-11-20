// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type validationBrokenWriter struct{ short bool }

func (w validationBrokenWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("PRIVATE-WRITER-CAUSE")
}

func adapterValidationResult() validation.Result {
	return validation.Result{
		Mode: validation.ModeFull, Verdict: validation.VerdictIncomplete,
		Scope:         validation.RuntimeScope{Devices: []validation.DeviceID{"serial-a"}, Hosts: []validation.HostID{"serial-a"}},
		RuntimeBudget: 7 * time.Minute,
		Checks: []validation.CheckResult{
			{CheckSpec: validation.CheckSpec{ID: "management/identity", Area: validation.AreaManagement, Subject: "Management cluster identity", Required: true}, Outcome: validation.Outcome{ID: "management/identity", State: validation.Passed}},
			{CheckSpec: validation.CheckSpec{ID: "tenant/ready", Area: validation.AreaTenant, Subject: "Tenant control plane", Required: true}, Outcome: validation.Outcome{ID: "tenant/ready", State: validation.Pending, Reason: validation.ReasonProgressing, Summary: "Tenant control plane is reconciling", Action: "Wait for the tenant control plane and rerun validation"}},
		},
		Report: validation.ReportDisposition{Status: validation.ReportPublished, Path: "/reports/validation.html"},
		Cause:  errors.New("PRIVATE-TRANSPORT-CAUSE"),
	}
}

var _ = Describe("standalone DPF validation adapter", func() {
	var stdout, stderr bytes.Buffer
	var request ValidateRequest
	var invocation target.Invocation
	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
		request = ValidateRequest{ValidationRequest: app.ValidationRequest{Inventory: "inventory.yaml", DeploymentFiles: "bundle", Mode: validation.ModeFull, ReportPath: "-"}, Stdout: &stdout, Stderr: &stderr}
		invocation = target.Invocation{Target: target.DPF, Phase: target.Validate, Output: target.OutputOptions{Format: "json"}}
	})

	It("rejects missing inputs, unsupported options and an absent runner before starting", func() {
		for _, mutate := range []func(*ValidateRequest){
			func(r *ValidateRequest) { r.Inventory = "" },
			func(r *ValidateRequest) { r.DeploymentFiles = "" },
			func(r *ValidateRequest) { r.Wait = -time.Second },
			func(r *ValidateRequest) { r.ConnectivityTimeout = -time.Second },
			func(r *ValidateRequest) { r.Mode = "unsupported" },
			func(r *ValidateRequest) { r.Mode = validation.ModeResources; r.ConnectivityTimeoutSet = true },
		} {
			r := request
			mutate(&r)
			_, err := NewValidateAdapter(r, func(context.Context, app.ValidationRequest) validation.Result {
				Fail("runner must not start")
				return validation.Result{}
			}).Bind(invocation)
			Expect(err).To(HaveOccurred())
		}
		_, err := NewValidateAdapter(request, nil).Bind(invocation)
		Expect(err).To(MatchError(ContainSubstring("runner is required")))
		invocation.Execution.DryRun = true
		_, err = NewValidateAdapter(request, NewValidateRunner()).Bind(invocation)
		Expect(err).To(MatchError(ContainSubstring("--dry-run")))
	})

	It("keeps validation isolated from other phases", func() {
		for _, phase := range []target.Phase{target.Discover, target.Generate, target.Deploy, target.Pipeline} {
			invocation.Phase = phase
			_, err := NewValidateAdapter(request, NewValidateRunner()).Bind(invocation)
			Expect(err).To(BeAssignableToTypeOf(&target.PhaseUnavailableError{}))
		}
		Expect(Descriptor().Capability(target.Validate).Available).To(BeTrue())
		Expect(Descriptor().Capability(target.Pipeline).Available).To(BeFalse())
	})

	It("writes exactly one canonical partial JSON document and keeps raw causes private", func() {
		expected := adapterValidationResult()
		operation, err := NewValidateAdapter(request, func(ctx context.Context, got app.ValidationRequest) validation.Result {
			Expect(got.Inventory).To(Equal(request.Inventory))
			Expect(got.ReportPath).To(Equal("-"))
			ui.FromContext(ctx).Info("Preparation summary")
			return expected
		}).Bind(invocation)
		Expect(err).NotTo(HaveOccurred())
		code, ok := apperrors.IsExitStatus(operation.Run(context.Background()))
		Expect(ok).To(BeTrue())
		Expect(code).To(Equal(4))
		encoded := stdout.String()
		Expect(encoded).NotTo(ContainSubstring("PRIVATE"))
		decoder := json.NewDecoder(strings.NewReader(encoded))
		var actual validation.Result
		Expect(decoder.Decode(&actual)).To(Succeed())
		Expect(actual.Verdict).To(Equal(expected.Verdict))
		Expect(actual.Report).To(Equal(expected.Report))
		Expect(actual.Checks).To(Equal(expected.Checks))
		Expect(decoder.Decode(new(any))).To(MatchError(io.EOF))
		Expect(stderr.String()).To(ContainSubstring("Preparation summary"))
		Expect(stderr.String()).NotTo(ContainSubstring("PRIVATE"))
	})

	It("shows concurrent progress on stderr and honors quiet", func() {
		var canonical string
		for _, quiet := range []bool{false, true} {
			stdout.Reset()
			stderr.Reset()
			invocation.Output.Quiet = quiet
			operation, err := NewValidateAdapter(request, func(_ context.Context, got app.ValidationRequest) validation.Result {
				result := adapterValidationResult()
				var wait sync.WaitGroup
				for range 4 {
					wait.Go(func() {
						for _, check := range result.Checks {
							got.Progress(validation.ProgressEvent{Check: check.CheckSpec, Phase: validation.Running})
							got.Progress(validation.ProgressEvent{Check: check.CheckSpec, Phase: validation.Completed, Outcome: check.Outcome})
						}
					})
				}
				wait.Wait()
				return result
			}).Bind(invocation)
			Expect(err).NotTo(HaveOccurred())
			Expect(operation.Run(context.Background())).NotTo(BeNil())
			if quiet {
				Expect(stderr.String()).To(BeEmpty())
				Expect(stdout.String()).To(Equal(canonical))
			} else {
				canonical = stdout.String()
				Expect(stderr.String()).To(ContainSubstring("RUNNING: Management cluster identity"))
				Expect(stderr.String()).To(ContainSubstring("PASS: Management cluster identity"))
				Expect(stderr.String()).To(ContainSubstring("PENDING: Tenant control plane"))
				Expect(strings.Count(stderr.String(), "Checking tenant cluster")).To(Equal(1))
			}
		}
	})

	It("keeps cancellation partial results and a finite exit category", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		operation, err := NewValidateAdapter(request, func(got context.Context, _ app.ValidationRequest) validation.Result {
			Expect(errors.Is(got.Err(), context.Canceled)).To(BeTrue())
			return validation.PreparationFailure(validation.ModeFull, validation.FatalCanceled, got.Err())
		}).Bind(invocation)
		Expect(err).NotTo(HaveOccurred())
		code, _ := apperrors.IsExitStatus(operation.Run(ctx))
		Expect(code).To(Equal(4))
		Expect(stdout.String()).To(ContainSubstring(`"fatal":"canceled"`))
		Expect(stdout.String()).NotTo(ContainSubstring("context canceled"))
	})

	It("preserves validation fatal precedence over failed or short stdout writes", func() {
		for _, short := range []bool{false, true} {
			for _, fatal := range []validation.FatalCategory{"", validation.FatalInput, validation.FatalAccess, validation.FatalCanceled} {
				stderr.Reset()
				request.Stdout = validationBrokenWriter{short: short}
				result := adapterValidationResult()
				result.Fatal = fatal
				if fatal == "" {
					result.Verdict = validation.VerdictPassed
				}
				operation, err := NewValidateAdapter(request, func(context.Context, app.ValidationRequest) validation.Result { return result }).Bind(invocation)
				Expect(err).NotTo(HaveOccurred())
				code, _ := apperrors.IsExitStatus(operation.Run(context.Background()))
				result.OutputFailed = true
				Expect(code).To(Equal(result.ExitCode()))
				Expect(code).NotTo(BeZero())
				Expect(stderr.String()).To(ContainSubstring("Cannot write complete DPF validation result"))
				Expect(stderr.String()).NotTo(ContainSubstring("PRIVATE"))
			}
		}
	})

	It("renders actionable text totals and a precise host-to-DPU relationship", func() {
		result := adapterValidationResult()
		invocation.Output.Format = "text"
		operation, err := NewValidateAdapter(request, func(context.Context, app.ValidationRequest) validation.Result { return result }).Bind(invocation)
		Expect(err).NotTo(HaveOccurred())
		Expect(operation.Run(context.Background())).NotTo(BeNil())
		text := stdout.String()
		Expect(text).To(Equal("DPF VALIDATION INCOMPLETE\n" +
			"Scope: full resource and runtime checks\n" +
			"Checks: 1 passed, 0 failed, 1 pending, 0 unverified, 0 skipped\n" +
			"Runtime budget: 7m0s\n" +
			"Selected hosts: host attached to DPU serial-a\n" +
			"PENDING: Tenant control plane — Tenant control plane is reconciling\n" +
			"  Next: Wait for the tenant control plane and rerun validation\n" +
			"Report: /reports/validation.html\n"))
		Expect(text).NotTo(ContainSubstring("management/identity"))
		Expect(text).NotTo(ContainSubstring("PRIVATE"))
	})

	It("labels resource-only success and preserves report failure independently", func() {
		result := adapterValidationResult()
		result.Mode = validation.ModeResources
		result.Verdict = validation.VerdictResourcesPassed
		result.Report = validation.ReportDisposition{Status: validation.ReportFailed}
		request.Mode = validation.ModeResources
		invocation.Output.Format = "text"
		operation, err := NewValidateAdapter(request, func(context.Context, app.ValidationRequest) validation.Result { return result }).Bind(invocation)
		Expect(err).NotTo(HaveOccurred())
		code, _ := apperrors.IsExitStatus(operation.Run(context.Background()))
		Expect(code).To(Equal(1))
		Expect(stdout.String()).To(ContainSubstring("RESOURCE CHECKS PASSED"))
		Expect(stdout.String()).To(ContainSubstring("probes excluded"))
		Expect(stdout.String()).To(ContainSubstring("Report: failed"))
		Expect(stdout.String()).NotTo(ContainSubstring("Report: /reports"))
	})
})
