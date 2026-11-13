// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func displayFixture() (inventory.Report, config.Input) {
	input := config.Input{}
	hostName, dpuName := "worker-a", "worker-a-dpu"
	input.Site.Discovery.Hosts = []config.Endpoint{{Name: &hostName, BMCEndpoint: "https://192.0.2.21"}}
	input.Site.Discovery.DPUs = []config.Endpoint{{Name: &dpuName, BMCEndpoint: "https://192.0.2.11"}}
	report := discovery.NewReport()
	report.Discovery.Outcome = "complete"
	report.Discovery.Mode = "inventory"
	report.Inventory = &inventory.Inventory{
		Hosts:        []inventory.Device{{ID: "host-id", RecordKey: "sha256:host-record", InputRef: "/discovery/hosts/0", BMCEndpoint: "https://192.0.2.21", ObservationState: "observed"}},
		DPUs:         []inventory.Device{{ID: "dpu-id", RecordKey: "sha256:dpu-record", InputRef: "/discovery/dpus/0", BMCEndpoint: "https://192.0.2.11", ObservationState: "observed", Model: "B4240", SerialNumber: "CARD-A"}},
		Associations: []inventory.Association{{DPUID: "dpu-id", HostID: "host-id", SerialNumber: "CARD-A"}},
	}
	report.Discovery.Checks = []inventory.Check{
		{ID: "dpf.management.bmc-administrator", Subject: "sha256:dpu-record", RequiredPhase: "discover", State: "passed", ReasonCode: "BMC_ADMIN_VERIFIED"},
		{ID: "dpf.inventory.association", Subject: "sha256:dpu-record", RequiredPhase: "discover", State: "passed"},
		{ID: "dpf.device.qualification", Subject: "sha256:host-record", RequiredPhase: "generate", State: "failed", ReasonCode: "QUALIFIED_ADAPTER_REQUIRED"},
		{ID: "dpf.device.qualification", Subject: "sha256:dpu-record", RequiredPhase: "generate", State: "failed", ReasonCode: "QUALIFIED_ADAPTER_REQUIRED"},
		{ID: "dpf.software.applicability", Subject: "sha256:dpu-record", RequiredPhase: "generate", State: "unknown", ReasonCode: "BOM_MODEL_UNSUPPORTED"},
		{ID: "dpf.management.path", Subject: "management", RequiredPhase: "generate", State: "not-requested", ReasonCode: "INVENTORY_MODE"},
		{ID: "dpf.management.dpf-authentication", Subject: "sha256:dpu-record", RequiredPhase: "deploy", State: "not-evaluated", ReasonCode: "DPF_AUTHENTICATION_NOT_EVALUATED"},
	}
	report.Artifacts = []inventory.Artifact{{Kind: "inventory", Path: "/results/discovered.yaml"}, {Kind: "report", Path: "/results/report.json"}}
	return report, input
}

var _ = Describe("human DPF discovery report", func() {
	It("shows named mappings and separates discovery success from later-stage gaps without mutating evidence", func() {
		report, input := displayFixture()
		before, err := json.Marshal(report)
		Expect(err).NotTo(HaveOccurred())
		var out bytes.Buffer
		Expect(textReport(&out, report, input)).To(Succeed())
		text := out.String()
		Expect(text).To(ContainSubstring("DPF discovery completed (inventory)"))
		Expect(text).To(ContainSubstring("Required discovery checks: 2/2 passed"))
		Expect(text).To(ContainSubstring("Identified: 1/1 hosts, 1/1 DPUs; host-to-DPU mappings: 1"))
		Expect(text).To(ContainSubstring("worker-a-dpu"))
		Expect(text).To(ContainSubstring("CARD-A"))
		Expect(text).To(ContainSubstring("B4240"))
		Expect(text).To(ContainSubstring("verified"))
		Expect(text).To(ContainSubstring("Before provisioning (these do not fail inventory discovery):"))
		Expect(strings.Count(text, "Hardware/firmware qualification")).To(Equal(1))
		Expect(text).To(ContainSubstring("worker-a, worker-a-dpu:"))
		Expect(text).To(ContainSubstring("Not checked in this run:"))
		Expect(text).To(ContainSubstring("Software compatibility is unknown"))
		Expect(text).To(ContainSubstring("Detailed report: /results/report.json"))
		Expect(text).NotTo(ContainSubstring("sha256:"))
		Expect(text).NotTo(ContainSubstring("dpf.device.qualification"))
		Expect(text).NotTo(ContainSubstring("Problems preventing"))
		after, err := json.Marshal(report)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("keeps a required administrator failure blocking and names the affected DPU", func() {
		report, input := displayFixture()
		report.Discovery.Outcome = "partial"
		report.Discovery.Checks[0].State = "failed"
		report.Discovery.Checks[0].ReasonCode = "BMC_ADMIN_REQUIRED"
		report.Discovery.Checks[0].Message = "The BMC account lacks Administrator privileges."
		report.Artifacts = []inventory.Artifact{{Kind: "partial", Path: "/run/partial.yaml"}}
		var out bytes.Buffer
		Expect(textReport(&out, report, input)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("DPF discovery incomplete"))
		Expect(out.String()).To(ContainSubstring("Required discovery checks: 1/2 passed"))
		Expect(out.String()).To(ContainSubstring("Problems preventing successful discovery:"))
		Expect(out.String()).To(ContainSubstring("worker-a-dpu: The BMC account lacks Administrator privileges."))
		Expect(out.String()).To(ContainSubstring("not verified"))
		Expect(out.String()).To(ContainSubstring("Partial inventory: /run/partial.yaml"))
		Expect(discovery.ExitCode(report)).To(Equal(2))
	})

	It("does not claim overall success after a publication failure or cancellation", func() {
		for _, state := range []string{"failed", "cancelled"} {
			report, input := displayFixture()
			report.Execution = state
			report.Errors = []inventory.Problem{{Code: "PUBLICATION_FAILED", Message: "output changed during collection", ExitCode: 1}}
			var out bytes.Buffer
			Expect(textReport(&out, report, input)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("DPF discovery " + state))
			Expect(out.String()).NotTo(ContainSubstring("DPF discovery completed"))
			Expect(out.String()).To(ContainSubstring("output changed during collection [PUBLICATION_FAILED]"))
			Expect(out.String()).To(ContainSubstring("Inventory: /results/discovered.yaml"))
		}
	})

	It("handles invalid configuration, cleanup failure and unknown subjects without exposing hashes", func() {
		report := discovery.NewReport()
		report.Execution = "failed"
		report.Discovery.Cleanup.State = "incomplete"
		report.Errors = []inventory.Problem{{Code: "INVALID_CONFIGURATION", Message: "password reference is missing", ExitCode: 2}}
		report.Discovery.Checks = []inventory.Check{{Subject: "sha256:unknown", RequiredPhase: "discover", State: "unknown", Message: "Source could not be identified."}}
		var out bytes.Buffer
		Expect(textReport(&out, report, config.Input{})).To(Succeed())
		Expect(out.String()).To(ContainSubstring("Unidentified source: Source could not be identified."))
		Expect(out.String()).To(ContainSubstring("cleanup is incomplete"))
		Expect(out.String()).NotTo(ContainSubstring("sha256:"))
	})

	It("uses endpoints when names are absent and distinguishes duplicate names", func() {
		report, input := displayFixture()
		input.Site.Discovery.DPUs[0].Name = nil
		labels := reportLabels(input, report.Inventory)
		Expect(labels["sha256:dpu-record"]).To(Equal("https://192.0.2.11"))
		input.Site.Discovery.DPUs[0].Name = input.Site.Discovery.Hosts[0].Name
		labels = reportLabels(input, report.Inventory)
		Expect(labels["sha256:dpu-record"]).To(Equal("worker-a (https://192.0.2.11)"))
		Expect(labels["sha256:host-record"]).To(Equal("worker-a (https://192.0.2.21)"))
	})

	It("distinguishes names that collide after terminal sanitation and retains all source identities", func() {
		report, input := displayFixture()
		hostName, dpuName := "node\nx", "node\tx"
		input.Site.Discovery.Hosts[0].Name = &hostName
		input.Site.Discovery.DPUs[0].Name = &dpuName
		labels := reportLabels(input, report.Inventory)
		Expect(labels["sha256:host-record"]).To(Equal("node x (https://192.0.2.21)"))
		Expect(labels["sha256:dpu-record"]).To(Equal("node x (https://192.0.2.11)"))
		Expect(labels["/management"]).To(Equal("Management cluster"))
		// Even unknown subjects with the same fallback label remain two sources.
		var out bytes.Buffer
		writeChecks(&out, []inventory.Check{{Subject: "one", Message: "Unavailable"}, {Subject: "two", Message: "Unavailable"}}, labels)
		Expect(out.String()).To(ContainSubstring("Unidentified source, Unidentified source: Unavailable"))
	})

	It("does not let a host-derived DPU replace the host's display identity", func() {
		report, input := displayFixture()
		report.Inventory.DPUs[0].ReaderAdapter = "host-card-v1"
		report.Inventory.DPUs[0].InputRef = "/discovery/hosts/0"
		labels := reportLabels(input, report.Inventory)
		Expect(labels["/discovery/hosts/0"]).To(Equal("worker-a"))
		Expect(labels["sha256:dpu-record"]).To(Equal("DPU CARD-A (via worker-a)"))
	})

	It("keeps different version findings and removes terminal control characters", func() {
		report, input := displayFixture()
		report.Inventory.DPUs[0].Model = "B4240\n\x1b[31m"
		report.Discovery.Checks = []inventory.Check{
			{ID: "dpf.software.nic", State: "failed", RequiredPhase: "generate", Subject: "sha256:dpu-record", Message: "NIC version A is below minimum."},
			{ID: "dpf.software.nic", State: "failed", RequiredPhase: "generate", Subject: "sha256:dpu-record", Message: "NIC version B is below minimum."},
		}
		var out bytes.Buffer
		Expect(textReport(&out, report, input)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("NIC version A"))
		Expect(out.String()).To(ContainSubstring("NIC version B"))
		Expect(out.String()).NotTo(ContainSubstring("\x1b"))
		Expect(out.String()).NotTo(ContainSubstring("B4240\n"))
	})

	It("returns the output writer's failure", func() {
		report, input := displayFixture()
		Expect(textReport(failedWriter{}, report, input)).To(MatchError("write failed"))
	})
})

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
