// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
)

func TestDriver(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF target") }

var _ = Describe("target boundary", func() {
	It("reports one JSON document for invalid input and returns exit two", func() {
		var stdout, stderr bytes.Buffer
		d := NewDiscoverAdapter(Request{Stdout: &stdout, Stderr: &stderr})
		op, e := d.Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: "json"}})
		Expect(e).NotTo(HaveOccurred())
		e = op.Run(context.Background())
		code, ok := errors.IsExitStatus(e)
		Expect(ok).To(BeTrue())
		Expect(code).To(Equal(2))
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["target"]).To(Equal("dpf"))
		Expect(result["execution"]).To(Equal("failed"))
	})
	It("advertises implemented public workflows and keeps the composite pipeline unavailable", func() {
		for _, p := range []target.Phase{target.Discover, target.Generate, target.Deploy, target.Validate} {
			Expect(Descriptor().Capability(p).Available).To(BeTrue())
		}
		Expect(Descriptor().Capability(target.Pipeline).Available).To(BeFalse())
	})

	It("binds the workflow adapter only to its configured phase", func() {
		adapter := NewWorkflowAdapter(WorkflowRequest{Phase: target.Generate})
		_, e := adapter.Bind(target.Invocation{Target: target.DPF, Phase: target.Deploy})
		Expect(e).To(MatchError(ContainSubstring("does not bind phase")))
		_, e = adapter.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(e).NotTo(HaveOccurred())
	})

	It("retains the discovery adapter phase boundary", func() {
		_, e := NewDiscoverAdapter(Request{}).Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(e).To(HaveOccurred())
	})
	It("keeps live preparation status on stderr and suppresses it with quiet without changing JSON", func() {
		for _, quiet := range []bool{false, true} {
			var stdout, stderr bytes.Buffer
			op, err := NewDiscoverAdapter(Request{Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: "json", Quiet: quiet}})
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.ExitCodeFromError(op.Run(context.Background()))).To(Equal(2))
			var result map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
			Expect(result["execution"]).To(Equal("failed"))
			if quiet {
				Expect(stderr.String()).To(BeEmpty())
			} else {
				Expect(stderr.String()).To(ContainSubstring("[RUNNING] Configuration"))
				Expect(stderr.String()).To(ContainSubstring("[FAIL] Discovery preparation"))
			}
		}
	})
	It("returns a local failure when the human summary cannot be written", func() {
		var stderr bytes.Buffer
		op, err := NewDiscoverAdapter(Request{Stdout: failedWriter{}, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: "text", Quiet: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(errors.ExitCodeFromError(op.Run(context.Background()))).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("cannot write DPF discovery result"))
	})
})
