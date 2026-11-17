// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF release-specific enrollment API reads", func() {
	It("keeps physical inventory usable when the controller-specific endpoint is missing", func() {
		GinkgoT().Setenv("DPF_NETWORK_FUNCTION_PASSWORD", "fixture-password")
		server, _ := networkFunctionInventoryServer(nil, false, false)
		defer server.Close()
		device, _, err := (Collector{ProvisioningRelease: "26.8.0"}).Collect(context.Background(), networkFunctionInventoryTarget(server.URL), 3*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(device.ObservationState).To(Equal("observed"))
		Expect(device.SerialNumber).To(Equal("CARD-A"))
		Expect(device.Observations["dpf-provisioning-api"].State).To(Equal("unknown"))
		Expect(device.Observations["dpf-provisioning-api"].ReasonCode).To(Equal("DPF_PROVISIONING_API_NOT_EXPOSED"))
	})
	const chassisURI = "/redfish/v1/Chassis/BlueField_0"
	const functionURI = chassisURI + "/NetworkAdapters/BlueField_NIC_0/NetworkDeviceFunctions/0"
	fixture := func() (*networkGraphFixture, Resource) {
		system := Resource{URI: "/redfish/v1/Systems/selected", Document: Document{"SerialNumber": "CARD-A", "AssetTag": "MT_0000001774"}}
		f := newNetworkGraphFixture()
		f.docs[system.URI] = Document{"SerialNumber": "CARD-A", "AssetTag": "MT_0000001774"}
		f.docs[chassisURI] = Document{"SerialNumber": "CARD-A", "Model": "B4240", "ChassisType": "Card", "AssetTag": "MT_0000001774"}
		f.docs[functionURI] = Document{"Id": "0", "NetDevFuncType": "Ethernet", "Ethernet": Document{"PermanentMACAddress": "02:00:00:00:01:01"}}
		return f, system
	}
	It("checks actual controller paths and unchanged card/PSID/PF0 identity using reads only", func() {
		f, system := fixture()
		client, closeServer := networkGraphClient(f)
		defer closeServer()
		result := provisioningAPIReads(context.Background(), client, "26.8.0", true, "CARD-A", system)
		Expect(result.State).To(Equal("observed"))
		Expect(result.Values).To(Equal([]string{"26.8.0", "02:00:00:00:01:01"}))
		Expect(f.reads[chassisURI]).To(Equal(2))
		Expect(f.reads[functionURI]).To(Equal(2))
		for _, method := range f.methods {
			Expect(method).To(Equal(http.MethodGet))
		}
	})
	It("does not fall back from a present but invalid System AssetTag", func() {
		for _, value := range []string{"", "N/A"} {
			f, system := fixture()
			system.Document["AssetTag"] = value
			f.docs[system.URI]["AssetTag"] = value
			client, closeServer := networkGraphClient(f)
			DeferCleanup(closeServer)

			result := provisioningAPIReads(context.Background(), client, "26.8.0", true, "CARD-A", system)

			Expect(result.State).To(Equal("unknown"))
			Expect(result.ReasonCode).To(Equal("DPF_PROVISIONING_PSID_UNAVAILABLE"))
			Expect(f.reads[functionURI]).To(BeZero(), "PF0 should not be read after the selected PSID is invalid")
		}
	})
	It("uses a valid System AssetTag even when the unselected chassis tag differs", func() {
		f, system := fixture()
		f.docs[chassisURI]["AssetTag"] = "MT_0000001775"
		client, closeServer := networkGraphClient(f)
		defer closeServer()

		result := provisioningAPIReads(context.Background(), client, "26.8.0", true, "CARD-A", system)

		Expect(result.State).To(Equal("observed"))
	})
	It("rejects a change that switches DPF's selected PSID source", func() {
		f, system := fixture()
		system.Document = Document{"SerialNumber": "CARD-A"}
		f.docs[system.URI] = Document{"SerialNumber": "CARD-A"}
		f.mutate = func(uri string, n int, document Document) {
			if uri == system.URI && n == 1 {
				document["AssetTag"] = "MT_0000001774"
			}
		}
		client, closeServer := networkGraphClient(f)
		defer closeServer()

		result := provisioningAPIReads(context.Background(), client, "26.8.0", true, "CARD-A", system)

		Expect(result.State).To(Equal("unknown"))
		Expect(result.ReasonCode).To(Equal("DPF_PROVISIONING_API_CHANGED"))
	})
	DescribeTable("does not confuse generic inventory support with controller compatibility", func(edit func(*networkGraphFixture), code string) {
		f, system := fixture()
		edit(f)
		client, closeServer := networkGraphClient(f)
		defer closeServer()
		result := provisioningAPIReads(context.Background(), client, "26.8.0", true, "CARD-A", system)
		Expect(result.State).NotTo(Equal("observed"))
		Expect(result.ReasonCode).To(Equal(code))
		Expect(result.Message).NotTo(BeEmpty())
	},
		Entry("legacy missing chassis", func(f *networkGraphFixture) { delete(f.docs, chassisURI) }, "DPF_PROVISIONING_API_NOT_EXPOSED"),
		Entry("missing PF0", func(f *networkGraphFixture) { delete(f.docs, functionURI) }, "DPF_PROVISIONING_API_NOT_EXPOSED"),
		Entry("access denied", func(f *networkGraphFixture) { f.statuses[chassisURI] = 403 }, "ACCESS_DENIED"),
		Entry("different card", func(f *networkGraphFixture) { f.docs[chassisURI]["SerialNumber"] = "OTHER" }, "DPF_PROVISIONING_IDENTITY_MISMATCH"),
		Entry("missing MAC", func(f *networkGraphFixture) { delete(f.docs[functionURI], "Ethernet") }, "DPF_PROVISIONING_PF0_UNAVAILABLE"),
		Entry("changed card", func(f *networkGraphFixture) {
			f.mutate = func(uri string, n int, d Document) {
				if uri == chassisURI && n == 2 {
					d["SerialNumber"] = "OTHER"
				}
			}
		}, "DPF_PROVISIONING_API_CHANGED"),
		Entry("changed PF0", func(f *networkGraphFixture) {
			f.mutate = func(uri string, n int, d Document) {
				if uri == functionURI && n == 2 {
					d["Ethernet"] = Document{"PermanentMACAddress": "02:00:00:00:01:02"}
				}
			}
		}, "DPF_PROVISIONING_API_CHANGED"),
	)
})
