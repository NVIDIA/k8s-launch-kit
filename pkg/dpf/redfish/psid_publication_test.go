// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BF4 optional PSID publication", func() {
	DescribeTable("retains optional failures through YAML publication and the JSON report", func(system, card Document, expected, reason string) {
		observation := bf4PSID(system, card, system, card)
		device := inventory.Device{
			ID: "dpu-one", RecordKey: "record-one", InputRef: "/discovery/dpus/0",
			ObservationState: "observed", BMCEndpoint: "https://192.0.2.1",
			Model: "B4240", SerialNumber: "CARD-ONE", ReaderAdapter: "bluefield-card-v1",
			Observations: map[string]inventory.Observation{"psid": observation},
		}
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		source := filepath.Join(root, "site.yaml")
		Expect(os.WriteFile(source, []byte("credentials: {}\ndiscovery:\n  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: reader}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: reader}]\n"), 0600)).To(Succeed())
		input, err := config.Load(source)
		Expect(err).NotTo(HaveOccurred())
		for _, outcome := range []string{"complete", "partial"} {
			By(outcome + " publication")
			output := filepath.Join(root, outcome+".yaml")
			store, err := artifacts.New(input, output, true)
			Expect(err).NotTo(HaveOccurred())
			report := &inventory.Report{
				Inventory: &inventory.Inventory{FormatVersion: inventory.FormatVersion, DPUs: []inventory.Device{device}},
				Discovery: inventory.Discovery{Outcome: outcome}, Artifacts: []inventory.Artifact{},
			}
			Expect(store.Publish(context.Background(), report)).To(Succeed())
			if outcome == "partial" {
				output = filepath.Join(store.RunDir, "partial.yaml")
			}
			loaded, err := config.Load(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Site.Inventory.DPUs).To(HaveLen(1))
			fact, err := inventory.DeviceToOutput(loaded.Site.Inventory.DPUs[0], "dpus")
			Expect(err).NotTo(HaveOccurred())
			Expect(fact.ID).To(Equal(device.ID))
			Expect(fact.PSID).To(Equal(expected))
			if expected == "" {
				Expect(fact.Issues).To(ContainElement(HaveField("Code", reason)))
			} else {
				Expect(fact.Issues).To(BeEmpty())
			}
			data, err := os.ReadFile(filepath.Join(store.RunDir, "report.json"))
			Expect(err).NotTo(HaveOccurred())
			var detailed inventory.Report
			Expect(json.Unmarshal(data, &detailed)).To(Succeed())
			Expect(detailed.Inventory.DPUs).To(HaveLen(1))
			Expect(detailed.Inventory.DPUs[0].Observations["psid"].ReasonCode).To(Equal(reason))
			for _, path := range []string{output, filepath.Join(store.RunDir, "report.json")} {
				data, err := os.ReadFile(path)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(data)).NotTo(ContainSubstring("credential-canary"))
			}
			Expect(report.Inventory.DPUs[0].Observations["psid"]).To(Equal(observation))
		}
	},
		Entry("unrelated asset tag", Document{"AssetTag": "ABC123"}, Document{}, "", "PSID_INVALID"),
		Entry("unrelated canary", Document{"AssetTag": "credential-canary"}, Document{}, "", "PSID_INVALID"),
		Entry("33-character PSID", Document{"AssetTag": "MT_" + strings.Repeat("A", 30)}, Document{}, "", "PSID_INVALID"),
		Entry("64-character PSID", Document{"AssetTag": "MT_" + strings.Repeat("A", 61)}, Document{}, "", "PSID_INVALID"),
		Entry("invalid selected chassis value", Document{}, Document{"AssetTag": "ABC123"}, "", "PSID_INVALID"),
		Entry("valid boundary", Document{"AssetTag": "MT_" + strings.Repeat("A", 29)}, Document{}, "MT_"+strings.Repeat("A", 29), "PSID_OBSERVED"),
		Entry("valid chassis fallback", Document{}, Document{"AssetTag": "MT_000001"}, "MT_000001", "PSID_OBSERVED"),
		Entry("System precedence preserves the selected value", Document{"AssetTag": "MT_000001"}, Document{"AssetTag": "MT_000002"}, "MT_000001", "PSID_OBSERVED"),
		Entry("absent value", Document{}, Document{}, "", "PSID_NOT_EXPOSED"),
	)
})
