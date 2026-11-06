// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package addresslookup

import (
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
	"time"
)

func TestLookup(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF address candidates") }

var _ = Describe("Explicit MAC address lookup", func() {
	var r Request
	BeforeEach(func() {
		var err error
		r, err = Validate(Config{Sources: []Source{{File: "leases", Format: "dnsmasq-leases"}}, Devices: []Selector{{Name: "dpu1", Role: "arm", MAC: "02:00:00:00:00:01"}}})
		Expect(err).NotTo(HaveOccurred())
	})
	It("retains usable leases as candidates with provenance, not verified endpoints", func() {
		out, ev := Collect(r, time.Unix(100, 0), func(string, int64) ([]byte, error) { return []byte("200 02:00:00:00:00:01 192.0.2.3 ignored *\n"), nil }, nil)
		Expect(out.Candidates).To(HaveLen(1))
		Expect(ev).To(HaveLen(1))
		Expect(out.Observations["device.dpu1"].State).To(Equal("candidate"))
		Expect(out.Candidates[0].EvidenceRef).NotTo(BeEmpty())
	})
	It("reports expired, missing, unreadable and malformed sources distinctly", func() {
		for _, tc := range []struct {
			data, code string
			err        error
		}{{"99 02:00:00:00:00:01 192.0.2.3 * *", "ADDRESS_LEASE_EXPIRED", nil}, {"", "ADDRESS_NOT_FOUND", nil}, {"", "ADDRESS_SOURCE_UNREADABLE", fmt.Errorf("secret-bearing failure")}, {"bad data", "ADDRESS_SOURCE_INVALID", nil}} {
			out, _ := Collect(r, time.Unix(100, 0), func(string, int64) ([]byte, error) { return []byte(tc.data), tc.err }, nil)
			Expect(out.Candidates).To(BeEmpty())
			key := "device.dpu1"
			if tc.code == "ADDRESS_SOURCE_UNREADABLE" || tc.code == "ADDRESS_SOURCE_INVALID" {
				key = "source.0"
			}
			Expect(out.Observations[key].ReasonCode).To(Equal(tc.code))
			Expect(out.Observations[key].Message).NotTo(BeEmpty())
			Expect(out.Observations[key].Message).NotTo(ContainSubstring("secret-bearing"))
		}
	})
	It("retains disagreements and does not select an address", func() {
		out, _ := Collect(r, time.Unix(100, 0), func(string, int64) ([]byte, error) { return []byte("200 02:00:00:00:00:01 192.0.2.3 * *"), nil }, []Neighbor{{Address: "192.0.2.4", MAC: r.Devices[0].MAC, State: "REACHABLE", Source: "host", EvidenceRef: "public"}})
		Expect(out.Candidates).To(HaveLen(2))
		Expect(out.Observations["device.dpu1"].ReasonCode).To(Equal("ADDRESS_AMBIGUOUS"))
		Expect(out.Observations["device.dpu1"].Values).To(BeEmpty())
	})
	It("rejects unsupported sources and invalid selectors", func() {
		for _, c := range []Config{{}, {Devices: []Selector{{Name: "x", Role: "arm", MAC: "ff:ff:ff:ff:ff:ff"}}}, {Devices: []Selector{{Name: "x", Role: "host", MAC: "02:00:00:00:00:01"}}}, {Sources: []Source{{File: "x", Format: "guessed"}}, Devices: r.Devices}} {
			_, err := Validate(c)
			Expect(err).To(HaveOccurred())
		}
	})
	It("ignores stale neighbors and invalidates the whole malformed lease source", func() {
		out, _ := Collect(r, time.Unix(100, 0), func(string, int64) ([]byte, error) {
			return []byte("200 02:00:00:00:00:01 192.0.2.3 * *\nmalformed"), nil
		}, []Neighbor{{Address: "192.0.2.4", MAC: r.Devices[0].MAC, State: "STALE", Source: "host"}})
		Expect(out.Candidates).To(BeEmpty())
		Expect(out.Observations["source.0"].ReasonCode).To(Equal("ADDRESS_SOURCE_INVALID"))
	})
})
