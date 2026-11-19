// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF report HTML", func() {
	It("shows the measured and incomplete parts of a partial full run", func() {
		route := validation.DirectedDevicePair{Source: "dpu-a", Destination: "dpu-b"}
		traffic := validation.DirectedHostPair{Source: "host-a", Destination: "host-b"}
		result := validation.Result{
			Mode:          validation.ModeFull,
			Scope:         validation.RuntimeScope{Devices: []validation.DeviceID{"dpu-a", "dpu-b"}, Hosts: []validation.HostID{"host-a", "host-b"}},
			Verdict:       validation.VerdictIncomplete,
			RuntimeBudget: 10 * time.Minute,
			Fatal:         validation.FatalIdentity,
			Cause:         errors.New("raw credential sentinel"),
			Checks: []validation.CheckResult{
				{
					CheckSpec: validation.CheckSpec{ID: "raw identifier sentinel", Subject: "Management <region>", Area: validation.AreaManagement},
					Outcome:   validation.Outcome{State: validation.Passed, Summary: "API identity verified"},
				},
				{
					CheckSpec: validation.CheckSpec{ID: validation.BGPCheckID("dpu-a"), Subject: "dpu-a"},
					Outcome: validation.Outcome{
						State: validation.Passed, Summary: "Expected peers established",
						BGP: &validation.BGPFacts{ExpectedPeers: 2, EstablishedPeers: 2, LocalASN: 65101},
					},
				},
				{
					CheckSpec: validation.CheckSpec{ID: validation.RouteCheckID(route), Subject: "dpu-a to dpu-b"},
					Outcome: validation.Outcome{
						State: validation.Passed, Summary: "Remote prefix installed",
						Route: &validation.RouteFacts{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Installed: true},
					},
				},
				{
					CheckSpec: validation.CheckSpec{ID: validation.TrafficCheckID(traffic, false), Subject: "host-a to host-b"},
					Outcome: validation.Outcome{
						State: validation.Passed, Summary: "Ordinary IPv4 response received",
						Traffic: &validation.TrafficFacts{Transmitted: 3, Received: 3, PayloadBytes: 56},
					},
				},
				{
					CheckSpec: validation.CheckSpec{ID: validation.TrafficCheckID(traffic, true), Subject: "host-a to host-b"},
					Outcome:   validation.Outcome{State: validation.Unverified, Reason: validation.ReasonIdentityDrift, Action: "Restore the selected host"},
					PriorOutcome: &validation.Outcome{
						State: validation.Passed, Summary: "MTU-sized DF response received",
						Traffic: &validation.TrafficFacts{Transmitted: 3, Received: 3, PayloadBytes: 8972},
					},
				},
			},
		}

		var rendered bytes.Buffer
		Expect(validation.RenderHTML(&rendered, result, validation.ReportMetadata{
			Release: "24.<release>", UseCase: "HBN-BGP", Version: "v-test",
			GeneratedAt: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
		})).To(Succeed())
		html := rendered.String()
		for _, want := range []string{
			"VALIDATION INCOMPLETE", "Selected identities changed or could not be verified.",
			"24.&lt;release&gt;", "Management &lt;region&gt;", "2026-11-19 10:00:00 UTC",
			"Hosts (2)", "DPUs (2)", "10m0s", "<code>host-a</code>", "<code>dpu-a</code>",
			"BGP peers established 2 of 2 · local ASN 65101",
			"Installed routes by directed DPU pair", "Route <code>192.0.2.0/24</code> installed",
			"Ordinary ICMP by directed host pair", "ICMP packets transmitted 3 · received 3 · payload 56 bytes",
			"MTU-sized DF ICMP by directed host pair", "At measurement time: passed · MTU-sized DF response received",
			"ICMP packets transmitted 3 · received 3 · payload 8972 bytes", "<th>Details</th>",
			"identity drift", "Restore the selected host",
		} {
			Expect(html).To(ContainSubstring(want))
		}
		for _, absent := range []string{"raw credential sentinel", "raw identifier sentinel", "<region>", "<release>", "VALIDATION PASSED"} {
			Expect(html).NotTo(ContainSubstring(absent))
		}
	})

	It("distinguishes resource-only success and summarizes excluded runtime measurements", func() {
		result := validation.Result{
			Mode: validation.ModeResources, Verdict: validation.VerdictResourcesPassed,
			DisabledRuntime: &validation.RuntimeCoverage{
				BGP: 256, Routes: 65_280, OrdinaryTraffic: 65_280, DFTraffic: 65_280,
			},
			Checks: []validation.CheckResult{
				{
					CheckSpec: validation.CheckSpec{ID: "software", Area: validation.AreaSoftware, Subject: "DPU software", Required: true},
					Outcome:   validation.Outcome{State: validation.Passed},
				},
			},
		}
		for i := range 256 {
			result.Scope.Hosts = append(result.Scope.Hosts, validation.HostID(fmt.Sprintf("host-%03d", i)))
			result.Scope.Devices = append(result.Scope.Devices, validation.DeviceID(fmt.Sprintf("dpu-%03d", i)))
		}
		var rendered bytes.Buffer
		Expect(validation.RenderHTML(&rendered, result, validation.ReportMetadata{})).To(Succeed())
		html := rendered.String()
		for _, want := range []string{
			"RESOURCE CHECKS PASSED",
			"Requested resource-only scope",
			"Connectivity excluded by resource-only scope",
			"<dt>DPU BGP checks</dt><dd>256</dd>",
			"<dt>Directed installed route checks</dt><dd>65280</dd>",
			"<dt>Directed ordinary ICMP probes</dt><dd>65280</dd>",
			"<dt>Directed MTU-sized DF ICMP probes</dt><dd>65280</dd>",
		} {
			Expect(html).To(ContainSubstring(want))
		}
		Expect(html).NotTo(ContainSubstring("VALIDATION PASSED"))
		Expect(html).NotTo(ContainSubstring("<h2>Needs attention</h2>"))
		Expect(html).NotTo(ContainSubstring("<h2>BGP peers</h2>"))
		Expect(rendered.Len()).To(BeNumerically("<", 30_000))
	})

	It("shows a bounded list of next actions before the grouped tables", func() {
		result := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictFailed}
		for i := range 24 {
			state, reason := validation.Failed, validation.ReasonMissing
			switch i {
			case 0:
				state, reason = validation.Passed, validation.ReasonNone
			case 1:
				state, reason = validation.Skipped, validation.ReasonDisabled
			case 2:
				state, reason = validation.Unverified, validation.ReasonDisabled
			}
			result.Checks = append(result.Checks, validation.CheckResult{
				CheckSpec: validation.CheckSpec{
					ID: validation.CheckID(fmt.Sprintf("software-%02d", i)), Area: validation.AreaSoftware,
					Subject: fmt.Sprintf("DPF package %02d", i), Required: true,
				},
				Outcome: validation.Outcome{
					State: state, Reason: reason, Action: fmt.Sprintf("Restore <package-%02d>", i),
				},
			})
		}
		var rendered bytes.Buffer
		Expect(validation.RenderHTML(&rendered, result, validation.ReportMetadata{})).To(Succeed())
		html := rendered.String()
		_, afterAttentionHeading, found := strings.Cut(html, "<h2>Needs attention</h2>")
		Expect(found).To(BeTrue())
		attention, _, found := strings.Cut(afterAttentionHeading, "</section>")
		Expect(found).To(BeTrue())
		Expect(attention).To(ContainSubstring("Restore &lt;package-02&gt;"))
		Expect(attention).To(ContainSubstring("Restore &lt;package-13&gt;"))
		Expect(attention).To(ContainSubstring("Additional checks needing attention in the grouped tables below (10)."))
		for _, absent := range []string{"DPF package 00", "DPF package 01", "DPF package 14", "Restore <package-02>", "<details"} {
			Expect(attention).NotTo(ContainSubstring(absent))
		}
		Expect(html).To(ContainSubstring("<details class=\"dpf-checks\">"))
		Expect(html).To(ContainSubstring("Show 24 checks"))
		Expect(html).To(ContainSubstring("Restore &lt;package-14&gt;"))
	})

	It("keeps the bound mode and unavailable identities in a preparation failure", func() {
		result := validation.PreparationFailure(validation.ModeResources, validation.FatalInput, errors.New("private input location"))
		var rendered bytes.Buffer
		Expect(validation.RenderHTML(&rendered, result, validation.ReportMetadata{})).To(Succeed())
		Expect(rendered.String()).To(ContainSubstring("Selected deployment identities are unavailable"))
		Expect(rendered.String()).To(ContainSubstring("Validation input or preparation could not complete"))
		Expect(rendered.String()).To(ContainSubstring("<h2>Validation preparation</h2>"))
		Expect(rendered.String()).To(ContainSubstring("Requested resource-only scope"))
		Expect(rendered.String()).NotTo(ContainSubstring("Requested full scope"))
		Expect(rendered.String()).NotTo(ContainSubstring("Connectivity excluded by resource-only scope"))
		Expect(rendered.String()).NotTo(ContainSubstring("private input location"))

		rendered.Reset()
		unknown := validation.PreparationFailure(validation.ModeUnknown, validation.FatalInput, nil)
		Expect(validation.RenderHTML(&rendered, unknown, validation.ReportMetadata{})).To(Succeed())
		Expect(rendered.String()).NotTo(ContainSubstring("Requested resource-only scope"))
		Expect(rendered.String()).NotTo(ContainSubstring("Requested full scope"))
	})
})
