// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

type validationKamajiReader struct {
	*validationReadFake
	result managementprobe.KamajiCompatibilityResult
}

func (f *validationKamajiReader) observeKamajiCompatibility(context.Context, string, string) managementprobe.KamajiCompatibilityResult {
	return f.result
}

func TestValidationKamajiCompatibilityDependency(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		reason     string
		wrongScope bool
		want       validation.State
		fatal      bool
	}{
		{name: "qualified", state: "passed", reason: "KAMAJI_COMPATIBILITY_VERIFIED", want: validation.Passed},
		{name: "unsupported", state: "failed", reason: "KAMAJI_COMPATIBILITY_PROCESS_UNSUPPORTED", want: validation.Failed},
		{name: "incomplete", state: "unknown", reason: "KAMAJI_COMPATIBILITY_READ_INCOMPLETE", want: validation.Unverified},
		{name: "rollout progressing", state: "unknown", reason: "KAMAJI_COMPATIBILITY_ROLLOUT_PENDING", want: validation.Pending},
		{name: "different scope", state: "passed", reason: "KAMAJI_COMPATIBILITY_VERIFIED", wrongScope: true, want: validation.Unverified},
		{name: "unrecognized pass", state: "passed", reason: "invented", want: validation.Unverified},
		{name: "changed source", state: "unknown", reason: "KAMAJI_COMPATIBILITY_CHANGED", want: validation.Unverified, fatal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := validationResourceFixture(t)
			f := &validationKamajiReader{validationReadFake: &validationReadFake{}, result: managementprobe.KamajiCompatibilityResult{State: test.state, ReasonCode: test.reason, Message: "Current producer observation.", Release: o.desired.plan.Intent().Release, Namespace: o.desired.operatorNamespace}}
			if test.wrongScope {
				f.result.Namespace = "different"
			}
			const id validation.CheckID = "management/dependency/kamaji.compatibility"
			r := newValidationRecorder(o.validationDependencyChecks(), nil)
			err := o.observeValidationDependencies(context.Background(), f, r)
			if (err != nil) != test.fatal {
				t.Fatalf("fatal=%v want=%v: %v", err != nil, test.fatal, err)
			}
			if r.outcomes[id].State != test.want {
				t.Fatalf("outcome=%+v want=%s", r.outcomes[id], test.want)
			}
			if test.fatal && r.outcomes[id].Reason != validation.ReasonIdentityDrift {
				t.Fatalf("drift classification lost: %+v", r.outcomes[id])
			}
		})
	}
}
