// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type appValidationObserver struct {
	plan   validation.Plan
	state  validation.State
	reason validation.Reason
	read   func()
}

func (o appValidationObserver) ObserveResources(context.Context, validation.Progress) (validation.ResourceSnapshot, error) {
	if o.read != nil {
		o.read()
	}
	var snapshot validation.ResourceSnapshot
	for _, check := range o.plan.Input().Checks {
		if check.FullOnly {
			continue
		}
		snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: o.state, Reason: o.reason, Summary: "Controlled current observation"})
	}
	return snapshot, nil
}

func (o appValidationObserver) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	Fail("resource-only application invoked runtime")
	return nil, nil
}

func validationAppFixture() ValidationRequest {
	GinkgoHelper()
	site := deploymentSite()
	site.Site.Credentials["host"] = site.Site.Credentials["bmc"]
	site.Site.Discovery.Hosts[0].SSH.KnownHostsFile = "/must-not-open/known_hosts"
	accessDir := GinkgoT().TempDir()
	site.Site.Management.KubeconfigRef.File = filepath.Join(accessDir, "kubeconfig")
	Expect(os.WriteFile(site.Site.Management.KubeconfigRef.File, []byte(`apiVersion: v1
kind: Config
current-context: selected
contexts:
- name: selected
  context: {cluster: selected, user: selected}
clusters:
- name: selected
  cluster: {server: "https://api.example.test"}
users:
- name: selected
  user: {token: fixture-token}
- name: unused
  user: {client-key: unused-private-key}
`), 0600)).To(Succeed())
	input, retained := generatedUseCaseSite(site)
	dir, err := filepath.EvalSymlinks(filepath.Dir(input.Path))
	Expect(err).NotTo(HaveOccurred())
	inventory := filepath.Join(dir, "inventory.yaml")
	Expect(os.WriteFile(inventory, publicFixtureBytes(input), 0600)).To(Succeed())
	path := filepath.Join(dir, "bundle")
	_, err = retained.Publish(context.Background(), path)
	Expect(err).NotTo(HaveOccurred())
	return ValidationRequest{Inventory: inventory, DeploymentFiles: path, Mode: validation.ModeResources, Version: "validation-test"}
}

func validationAppFactory(state validation.State, reason validation.Reason, called *int, read func()) validationObserverFactory {
	return func(retained *bundle.Bundle, plan *generation.Plan, access config.Request, unavailable []config.AccessIssue, mode validation.Mode) (validation.Plan, validation.Observer, error) {
		*called++
		compiled, _, err := deployment.NewValidationObserver(retained, plan, access, unavailable, mode)
		return compiled, appValidationObserver{plan: compiled, state: state, reason: reason, read: read}, err
	}
}

var _ = Describe("standalone DPF validation application", func() {
	It("uses the real retained input preparation and observer compiler without running deployment effects", func() {
		r := validationAppFixture()
		before, err := bundle.Load(r.DeploymentFiles)
		Expect(err).NotTo(HaveOccurred())
		calls, reads := 0, 0
		result := validate(context.Background(), r, validationAppFactory(validation.Passed, validation.ReasonNone, &calls, func() { reads++ }))
		Expect(result.Cause).NotTo(HaveOccurred())
		Expect(calls).To(Equal(1))
		Expect(reads).To(Equal(1))
		Expect(result.Verdict).To(Equal(validation.VerdictResourcesPassed))
		Expect(result.ExitCode()).To(BeZero())
		Expect(result.Report.Status).To(Equal(validation.ReportPublished))
		Expect(filepath.Dir(result.Report.Path)).NotTo(Equal(r.DeploymentFiles))
		report, err := os.ReadFile(result.Report.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(report)).To(ContainSubstring("RESOURCE CHECKS PASSED"))
		Expect(string(report)).To(ContainSubstring("hbn-bgp"))
		Expect(string(report)).To(ContainSubstring("validation-test"))
		info, err := os.Stat(result.Report.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
		after, err := bundle.Load(r.DeploymentFiles)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Index().BundleDigest).To(Equal(before.Index().BundleDigest))
	})

	It("publishes a partial private report for malformed inputs without exposing raw error data", func() {
		r := validationAppFixture()
		Expect(os.WriteFile(r.Inventory, []byte("credentials: [private-canary"), 0600)).To(Succeed())
		calls := 0
		result := validate(context.Background(), r, validationAppFactory(validation.Passed, "", &calls, nil))
		Expect(calls).To(BeZero())
		Expect(result.ExitCode()).To(Equal(2))
		Expect(result.Report.Status).To(Equal(validation.ReportPublished))
		report, err := os.ReadFile(result.Report.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(report)).NotTo(ContainSubstring("private-canary"))
		Expect(string(report)).To(ContainSubstring("Inventory and overrides"))
	})

	It("keeps canceled preparation partial without opening its inventory", func() {
		r := validationAppFixture()
		r.Inventory = "/must-not-open/inventory"
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		result := validate(ctx, r, validationAppFactory(validation.Passed, "", &calls, nil))
		Expect(calls).To(BeZero())
		Expect(errors.Is(result.Cause, context.Canceled)).To(BeTrue())
		Expect(result.ExitCode()).To(Equal(4))
		Expect(result.Report.Status).To(Equal(validation.ReportPublished))
	})

	It("honors disabled HTML even when inputs cannot be prepared", func() {
		result := Validate(context.Background(), ValidationRequest{Inventory: "/missing", DeploymentFiles: "/missing-bundle", ReportPath: "-", Mode: validation.ModeResources})
		Expect(result.ExitCode()).To(Equal(2))
		Expect(result.Report.Status).To(Equal(validation.ReportDisabled))
	})

	It("rejects unsafe explicit output before observation and preserves the input bytes", func() {
		r := validationAppFixture()
		r.ReportPath = r.Inventory
		before, err := os.ReadFile(r.Inventory)
		Expect(err).NotTo(HaveOccurred())
		calls := 0
		result := validate(context.Background(), r, validationAppFactory(validation.Passed, "", &calls, nil))
		Expect(calls).To(BeZero())
		Expect(result.ExitCode()).To(Equal(2))
		Expect(result.Report.Status).To(Equal(validation.ReportUnavailable))
		after, err := os.ReadFile(r.Inventory)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("protects unused kubeconfig credential paths while permitting a separate explicit report", func() {
		r := validationAppFixture()
		input, err := config.LoadInputs(r.Inventory, "")
		Expect(err).NotTo(HaveOccurred())
		keyPath := filepath.Join(filepath.Dir(input.Site.Management.KubeconfigRef.File), "unused-private-key")
		Expect(os.WriteFile(keyPath, []byte("private-key-canary"), 0600)).To(Succeed())
		r.ReportPath = keyPath
		calls := 0
		result := validate(context.Background(), r, validationAppFactory(validation.Passed, "", &calls, nil))
		Expect(calls).To(BeZero())
		Expect(result.ExitCode()).To(Equal(2))
		data, err := os.ReadFile(keyPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("private-key-canary"))
		r.ReportPath = filepath.Join(filepath.Dir(r.DeploymentFiles), "separate-report.html")
		result = validate(context.Background(), r, validationAppFactory(validation.Passed, "", &calls, nil))
		Expect(result.Cause).NotTo(HaveOccurred())
		Expect(calls).To(Equal(1))
		Expect(result.ExitCode()).To(BeZero())
		Expect(result.Report.Path).To(Equal(r.ReportPath))
	})

	It("rejects timeout mode and presence mistakes before observer construction", func() {
		for _, timeout := range []time.Duration{0, -time.Second, time.Minute} {
			r := validationAppFixture()
			r.ConnectivityTimeoutSet, r.ConnectivityTimeout = true, timeout
			calls := 0
			result := validate(context.Background(), r, validationAppFactory(validation.Passed, "", &calls, nil))
			Expect(calls).To(BeZero())
			Expect(result.ExitCode()).To(Equal(2))
		}
	})

	It("retains validation failure when the prepared report directory is removed during observation", func() {
		for _, state := range []validation.State{validation.Passed, validation.Failed} {
			r := validationAppFixture()
			calls := 0
			reason := validation.ReasonNone
			if state == validation.Failed {
				reason = validation.ReasonHealth
			}
			result := validate(context.Background(), r, validationAppFactory(state, reason, &calls, func() {
				entries, err := os.ReadDir(filepath.Dir(r.DeploymentFiles))
				Expect(err).NotTo(HaveOccurred())
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "bundle-validation-") {
						Expect(os.RemoveAll(filepath.Join(filepath.Dir(r.DeploymentFiles), entry.Name()))).To(Succeed())
					}
				}
			}))
			Expect(calls).To(Equal(1))
			Expect(result.Report.Status).To(Equal(validation.ReportFailed))
			if state == validation.Passed {
				Expect(result.Verdict).To(Equal(validation.VerdictResourcesPassed))
				Expect(result.ExitCode()).To(Equal(1))
			} else {
				Expect(result.Verdict).To(Equal(validation.VerdictFailed))
				Expect(result.ExitCode()).To(Equal(4))
			}
		}
	})
})
