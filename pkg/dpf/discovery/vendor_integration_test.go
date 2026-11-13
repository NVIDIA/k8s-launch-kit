// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Only the DPU side is fixed: the host uses the real HTTPS/session/ownership
// collector, so acceptance depends on normalized HPE PCI evidence.
type vendorBindingCollector struct{}

func (vendorBindingCollector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	if target.Role == "hosts" {
		return (redfish.Collector{}).Collect(ctx, target, timeout)
	}
	return (fixtureCollector{admin: true}).Collect(ctx, target, timeout)
}

var _ = Describe("HPE PCI vendor evidence through discovery publication", func() {
	It("publishes NVIDIA and unknown vendors but preserves the handoff after a same-serial vendor contradiction", func() {
		var vendor atomic.Int64
		vendor.Store(0x15b3)
		link := func(uri string) redfish.Document { return redfish.Document{"@odata.id": uri} }
		collection := func(uris ...string) redfish.Document {
			members := []any{}
			for _, uri := range uris {
				members = append(members, link(uri))
			}
			return redfish.Document{"Members@odata.count": len(uris), "Members": members}
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const sessions = "/redfish/v1/SessionService/Sessions"
			if r.Method == http.MethodPost && r.URL.Path == sessions {
				w.Header().Set("X-Auth-Token", "fixture-session")
				w.Header().Set("Location", sessions+"/owned")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			if r.Method == http.MethodDelete && r.URL.Path == sessions+"/owned" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var doc redfish.Document
			switch r.URL.Path {
			case "/redfish/v1/":
				doc = redfish.Document{"SessionService": link("/redfish/v1/SessionService"), "Systems": link("/redfish/v1/Systems")}
			case "/redfish/v1/SessionService":
				doc = redfish.Document{"Sessions": link(sessions)}
			case "/redfish/v1/Systems":
				doc = collection("/redfish/v1/Systems/host")
			case "/redfish/v1/Systems/host":
				doc = redfish.Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "HPE", "Model": "ProLiant", "Links": redfish.Document{"Chassis": []any{link("/redfish/v1/Chassis/host")}}, "Oem": redfish.Document{"Hpe": redfish.Document{"Links": redfish.Document{"PCIDevices": []any{link("/redfish/v1/Systems/host/PCIDevices")}}}}}
			case "/redfish/v1/Chassis/host":
				doc = redfish.Document{"NetworkAdapters": link("/redfish/v1/Chassis/host/NetworkAdapters")}
			case "/redfish/v1/Chassis/host/NetworkAdapters":
				doc = collection("/redfish/v1/Chassis/host/NetworkAdapters/selected", "/redfish/v1/Chassis/host/NetworkAdapters/other")
			case "/redfish/v1/Chassis/host/NetworkAdapters/selected":
				doc = redfish.Document{"Id": "selected", "SerialNumber": "CARD-ONE", "Model": "Network adapter"}
			case "/redfish/v1/Chassis/host/NetworkAdapters/other":
				doc = redfish.Document{"Id": "other", "SerialNumber": "OTHER-NIC", "Model": "Intel E810"}
			case "/redfish/v1/Systems/host/PCIDevices":
				doc = collection("/redfish/v1/Systems/host/PCIDevices/selected", "/redfish/v1/Systems/host/PCIDevices/other")
			case "/redfish/v1/Systems/host/PCIDevices/selected":
				doc = redfish.Document{"ClassCode": 2, "DeviceResourceId": "selected", "SegmentNumber": 0, "BusNumber": 129, "DeviceNumber": 0, "FunctionNumber": 0, "DeviceID": 41695}
				if value := vendor.Load(); value >= 0 {
					doc["VendorID"] = value
				}
			case "/redfish/v1/Systems/host/PCIDevices/other":
				doc = redfish.Document{"ClassCode": 2, "DeviceResourceId": "other", "VendorID": 0x8086}
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		}))
		DeferCleanup(server.Close)
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		caPath := filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)).To(Succeed())
		Expect(os.Setenv("DPF_VENDOR_BINDING_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_VENDOR_BINDING_PASSWORD")
		seed := fmt.Sprintf("credentials:\n  reader: {username: reader, passwordRef: {env: DPF_VENDOR_BINDING_PASSWORD}}\ndiscovery:\n  tls: {caFile: '%s'}\n  hosts: [{bmcEndpoint: '%s', credentialRef: reader}]\n  dpus: [{bmcEndpoint: 'https://dpu.invalid', credentialRef: reader}]\n  probes: {mode: inventory}\n", caPath, server.URL)
		inputPath := filepath.Join(dir, "seed.yaml")
		Expect(os.WriteFile(inputPath, []byte(seed), 0600)).To(Succeed())
		in, err := config.Load(inputPath)
		Expect(err).NotTo(HaveOccurred())
		req, err := config.Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		output := filepath.Join(dir, "discovered.yaml")
		var previous []byte
		for _, value := range []int64{0x15b3, -1, 0x8086} {
			vendor.Store(value)
			store, err := artifacts.New(in, output, true)
			Expect(err).NotTo(HaveOccurred())
			report := Collect(context.Background(), req, store.RunID(), Dependencies{BMC: vendorBindingCollector{}})
			Expect(report.Errors).To(BeEmpty())
			Expect(report.Inventory.Hosts[0].Adapters).To(HaveLen(2))
			var selected inventory.Adapter
			for _, adapter := range report.Inventory.Hosts[0].Adapters {
				if adapter.SerialNumber == "CARD-ONE" {
					selected = adapter
				} else {
					Expect(adapter.VendorID).To(Equal("8086"))
					Expect(adapter.Selection).To(Equal("not-selected"))
				}
			}
			Expect(selected.PCIAddress).To(Equal("0000:81:00.0"))
			Expect(store.Publish(context.Background(), &report)).To(Succeed())
			current, err := os.ReadFile(output)
			Expect(err).NotTo(HaveOccurred())
			if value == 0x8086 {
				Expect(selected.VendorID).To(Equal("8086"))
				Expect(selected.Selection).To(Equal("not-selected"))
				Expect(ExitCode(report)).To(Equal(2))
				Expect(report.Discovery.Outcome).To(Equal("partial"))
				Expect(report.Inventory.Associations).To(BeEmpty())
				Expect(current).To(Equal(previous))
				var check inventory.Check
				for _, c := range report.Discovery.Checks {
					if c.ReasonCode == "HOST_ADAPTER_VENDOR_CONFLICT" {
						check = c
					}
				}
				Expect(check.State).To(Equal("failed"))
				Expect(check.Message).To(ContainSubstring("Verify the selected endpoints"))
				Expect(check.EvidenceRefs).To(BeEmpty())
				Expect(selected.EvidenceRefs).To(BeEmpty())
				Expect(report.Inventory.Evidence).To(BeEmpty())
				Expect(report.Artifacts).To(ContainElement(HaveField("Path", filepath.Join(store.RunDir, "partial.yaml"))))
				persisted, err := os.ReadFile(filepath.Join(store.RunDir, "report.json"))
				Expect(err).NotTo(HaveOccurred())
				Expect(string(persisted)).To(ContainSubstring("HOST_ADAPTER_VENDOR_CONFLICT"))
				Expect(string(persisted)).NotTo(ContainSubstring(`"evidenceRefs"`))
				Expect(string(persisted)).NotTo(ContainSubstring(`"evidence"`))
				Expect(string(persisted)).NotTo(ContainSubstring("fixture-password"))
			} else {
				if value < 0 {
					Expect(selected.VendorID).To(BeEmpty())
				} else {
					Expect(selected.VendorID).To(Equal("15b3"))
				}
				Expect(ExitCode(report)).To(Equal(0))
				Expect(report.Discovery.Outcome).To(Equal("complete"))
				Expect(report.Inventory.Associations).To(HaveLen(1))
				previous = current
			}
		}
	})
})
