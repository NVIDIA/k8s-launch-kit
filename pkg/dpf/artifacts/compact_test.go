// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("compact device artifact handoff", func() {
	It("publishes diagnostics only while preserving consumer facts and comparisons", func() {
		for _, complete := range []bool{true, false} {
			dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			seed := filepath.Join(dir, "site.yaml")
			Expect(os.WriteFile(seed, []byte("credentials: {}\ndiscovery: {hosts: [], dpus: []}\n"), 0600)).To(Succeed())
			input, err := config.Load(seed)
			Expect(err).NotTo(HaveOccurred())
			software, err := releases.Resolve("26.4.0")
			Expect(err).NotTo(HaveOccurred())
			inv := &inventory.Inventory{
				FormatVersion: inventory.FormatVersion, Software: &software,
				DPUs: []inventory.Device{{
					ID: "dpu-one", RecordKey: "dpu-source", BMCEndpoint: "https://dpu.invalid", SerialNumber: "CARDONE", Model: "B3210", ObservationState: "observed",
					Observations: map[string]inventory.Observation{
						"bmc-administrator":          {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"},
						"psid":                       {State: "observed", Values: []string{"MT_0000000001"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"},
						"firmware-inventory":         {State: "observed"},
						"managers":                   {State: "observed"},
						"bmc-ntp":                    {State: "unavailable", ReasonCode: "ACCESS_DENIED", Message: "NTP read denied."},
						"missing-fact":               {State: "unknown", ReasonCode: "PROPERTY_NOT_EXPOSED", Message: "Requested property is absent."},
						"client-certificate-support": {State: "unknown", ReasonCode: "NOT_EVALUATED", Message: "Deferred to provisioning."},
					},
					Firmware: []inventory.Firmware{{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "running", ResourceURI: "/redfish/v1/UpdateService/FirmwareInventory/NIC"}},
				}},
				Hosts:        []inventory.Device{{ID: "host-one", RecordKey: "host-source", SystemUUID: "00000000-0000-0000-0000-000000000001", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "slot-one", SerialNumber: "CARDONE"}}}},
				Associations: []inventory.Association{{DPUID: "dpu-one", HostID: "host-one", HostAdapterKey: "slot-one", SerialNumber: "CARDONE"}},
			}
			beforeChecks := discovery.SoftwareChecks(inv)
			Expect(beforeChecks).To(ContainElement(And(HaveField("ID", "dpf.software.nic"), HaveField("State", "passed"))))
			outcome := "complete"
			if !complete {
				outcome = "partial"
				// Identity can already exist when a final recheck fails. Never
				// infer success from that ID when reading the partial result.
				inv.Hosts[0].ObservationState = "unavailable"
				inv.Hosts[0].ReasonCode = "IDENTITY_CHANGED"
				beforeChecks = discovery.SoftwareChecks(inv)
			}
			r := &inventory.Report{SchemaVersion: inventory.FormatVersion, Inventory: inv, Discovery: inventory.Discovery{Outcome: outcome, Checks: beforeChecks}}
			store, err := New(input, filepath.Join(dir, "discovered.yaml"), true)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.Publish(context.Background(), r)).To(Succeed())
			path := store.Output
			if !complete {
				path = filepath.Join(store.RunDir, "partial.yaml")
			}
			data, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			var wire map[string]any
			Expect(yaml.Unmarshal(data, &wire)).To(Succeed())
			assertCompactDeviceWire(wire["inventory"].(map[string]any), complete)
			loaded, err := config.Load(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(discovery.SoftwareChecks(loaded.Site.Inventory)).To(Equal(beforeChecks))
			Expect(discovery.Diff(inv, loaded.Site.Inventory)).To(BeEmpty())
			Expect(loaded.Site.Inventory.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
			data, err = os.ReadFile(filepath.Join(store.RunDir, "report.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Unmarshal(data, &wire)).To(Succeed())
			assertCompactDeviceWire(wire["inventory"].(map[string]any), complete)
			var report inventory.Report
			Expect(json.Unmarshal(data, &report)).To(Succeed())
			Expect(discovery.SoftwareChecks(report.Inventory)).To(Equal(beforeChecks))
			Expect(discovery.Diff(inv, report.Inventory)).To(BeEmpty())
			beforeJSON, err := json.Marshal(beforeChecks)
			Expect(err).NotTo(HaveOccurred())
			afterJSON, err := json.Marshal(report.Discovery.Checks)
			Expect(err).NotTo(HaveOccurred())
			Expect(afterJSON).To(MatchJSON(beforeJSON))
			// Publication is a projection, never a mutation of the collector data.
			Expect(inv.DPUs[0].ObservationState).To(Equal("observed"))
			Expect(inv.DPUs[0].Observations).To(HaveKey("managers"))
			Expect(inv.DPUs[0].Observations).To(HaveKey("psid"))
		}
	})
})

func assertCompactDeviceWire(inv map[string]any, complete bool) {
	dpu := inv["dpus"].([]any)[0].(map[string]any)
	Expect(dpu).NotTo(HaveKey("observationState"))
	Expect(dpu["psid"]).To(Equal("MT_0000000001"))
	Expect(dpu["authentication"].(map[string]any)["bmcAdministrator"]).To(Equal("verified"))
	observations := dpu["observations"].(map[string]any)
	Expect(observations).To(HaveLen(2))
	Expect(observations).To(HaveKey("bmc-ntp"))
	Expect(observations).To(HaveKey("missing-fact"))
	for _, item := range observations {
		Expect(item.(map[string]any)).NotTo(HaveKey("values"))
	}
	host := inv["hosts"].([]any)[0].(map[string]any)
	Expect(host).NotTo(HaveKey("firmware"))
	Expect(host).NotTo(HaveKey("time"))
	if complete {
		Expect(host).NotTo(HaveKey("observationState"))
	} else {
		Expect(host["observationState"]).To(Equal("unavailable"))
		Expect(host["reasonCode"]).To(Equal("IDENTITY_CHANGED"))
	}
}
