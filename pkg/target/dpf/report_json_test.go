// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type failedReportWriter struct {
	calls int
	short bool
}

func (w *failedReportWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.short {
		return len(data) - 1, nil
	}
	return 0, errors.New("closed output")
}

var _ = Describe("JSON discovery failure output", func() {
	It("writes one error document when measured inventory cannot be encoded", func() {
		report := discovery.NewReport()
		report.Discovery.Outcome = "complete"
		report.Discovery.Mode = "inventory"
		report.Errors = []inventory.Problem{{Code: "PUBLICATION_FAILED", Message: "Inventory encoding failed", ExitCode: 1}}
		report.Inventory = &inventory.Inventory{FormatVersion: inventory.FormatVersion, Hosts: []inventory.Device{{RecordKey: "host", SSHEndpoint: "ssh://host.example:22", Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{PCIAddress: "0000:01:00.0", Domain: "host-pci", Firmware: inventory.Observation{State: "observed", Values: []string{"running=1.2.3", "stored=1.2.4"}, Tool: "devlink"}}}}}}}}
		var out bytes.Buffer
		err := writeJSONReport(&out, report)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("firmwareVersion"))
		decoder := json.NewDecoder(&out)
		var result inventory.Report
		Expect(decoder.Decode(&result)).To(Succeed())
		Expect(result.Inventory).To(BeNil())
		Expect(result.Execution).To(Equal("failed"))
		Expect(result.Discovery.Outcome).To(Equal("complete"))
		Expect(result.Errors).To(HaveLen(2))
		Expect(result.Errors[0].Code).To(Equal("PUBLICATION_FAILED"))
		Expect(result.Errors[1].Code).To(Equal("RESULT_ENCODING_FAILED"))
		Expect(result.Errors[1].Message).To(ContainSubstring("requires exactly one nonempty measured value"))
		Expect(decoder.Decode(&result)).To(MatchError(io.EOF))
		Expect(report.Inventory).NotTo(BeNil())
		Expect(report.Errors).To(HaveLen(1))
	})
	It("does not retry a failed output write or change a valid report", func() {
		report := discovery.NewReport()
		var out bytes.Buffer
		Expect(writeJSONReport(&out, report)).To(Succeed())
		var result inventory.Report
		Expect(json.Unmarshal(out.Bytes(), &result)).To(Succeed())
		Expect(result).To(Equal(report))
		writer := &failedReportWriter{}
		Expect(writeJSONReport(writer, report)).To(MatchError("write discovery report: closed output"))
		Expect(writer.calls).To(Equal(1))
		short := &failedReportWriter{short: true}
		Expect(writeJSONReport(short, report)).To(MatchError("write discovery report: short write"))
		Expect(short.calls).To(Equal(1))
	})
})
