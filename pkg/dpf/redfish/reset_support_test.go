// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("release-owned static host reset support", func() {
	const uri = "/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_BMC_0"
	It("checks the observed build twice through reads only, without requiring a shut-down DPU", func() {
		f := newNetworkGraphFixture()
		f.docs[uri] = Document{"Id": "BlueField_FW_BMC_0", "@odata.id": uri, "Version": "BF4-26.10-0001"}
		client, closeServer := networkGraphClient(f)
		defer closeServer()
		observed := resetSupportReads(context.Background(), client, "26.8.0", "B4240")
		Expect(ResetSupportVerified(inventory.Device{Model: "B4240", Observations: map[string]inventory.Observation{"host-reset-support": observed}}, "26.8.0")).To(BeTrue())
		Expect(ResetSupportVerified(inventory.Device{Model: "B4240", Observations: map[string]inventory.Observation{"host-reset-support": observed}}, "26.4.0")).To(BeFalse())
		Expect(f.reads[uri]).To(Equal(2))
		Expect(f.methods).To(Equal([]string{http.MethodGet, http.MethodGet}))
		observed.Values[3] = "BF4-other"
		Expect(ResetSupportVerified(inventory.Device{Model: "B4240", Observations: map[string]inventory.Observation{"host-reset-support": observed}}, "26.8.0")).To(BeFalse())
	})
	It("rejects unsupported models or releases before any API operation", func() {
		for _, input := range []struct{ release, model string }{{"", "B4240"}, {"26.4.0", "B4240"}, {"26.8.0", "BlueField-4"}, {"26.8.0", "other"}} {
			f := newNetworkGraphFixture()
			client, closeServer := networkGraphClient(f)
			observed := resetSupportReads(context.Background(), client, input.release, input.model)
			Expect(observed.State).To(Equal("failed"))
			Expect(f.methods).To(BeEmpty())
			closeServer()
		}
	})
	It("keeps absent, denied, changed and unsupported firmware ineligible", func() {
		for _, scenario := range []string{"absent", "denied", "changed", "unsupported", "missing-id", "wrong-id", "wrong-uri", "changed-identity"} {
			f := newNetworkGraphFixture()
			f.docs[uri] = Document{"Id": "BlueField_FW_BMC_0", "@odata.id": uri, "Version": "BF4-26.10-0001"}
			switch scenario {
			case "absent":
				delete(f.docs[uri], "Version")
			case "denied":
				f.statuses[uri] = http.StatusForbidden
			case "changed":
				f.mutate = func(_ string, n int, d Document) {
					if n == 2 {
						d["Version"] = "BF4-changed"
					}
				}
			case "missing-id":
				delete(f.docs[uri], "Id")
			case "wrong-id":
				f.docs[uri]["Id"] = "some-other-resource"
			case "wrong-uri":
				f.docs[uri]["@odata.id"] = "/redfish/v1/UpdateService/FirmwareInventory/other"
			case "changed-identity":
				f.mutate = func(_ string, n int, d Document) {
					if n == 2 {
						d["Id"] = "other"
					}
				}
			case "unsupported":
				f.docs[uri]["Version"] = "BF4-older"
			}
			client, closeServer := networkGraphClient(f)
			observed := resetSupportReads(context.Background(), client, "26.8.0", "B4240")
			Expect(ResetSupportVerified(inventory.Device{Model: "B4240", Observations: map[string]inventory.Observation{"host-reset-support": observed}}, "26.8.0")).To(BeFalse(), scenario)
			Expect(observed.State).NotTo(Equal("observed"), scenario)
			Expect(observed.ReasonCode).NotTo(BeEmpty())
			closeServer()
		}
	})
})
