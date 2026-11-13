// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type softwareReader struct {
	docs        map[string]Document
	collections map[string][]Resource
	failures    map[string]error
}

func (r softwareReader) Get(_ context.Context, uri string) (Document, error) {
	return r.docs[uri], r.failures[uri]
}
func (r softwareReader) Collection(_ context.Context, uri string) ([]Resource, error) {
	return r.collections[uri], r.failures[uri]
}
func softwareFixture() (softwareReader, Document, Resource) {
	return softwareReader{
		docs:        map[string]Document{"/redfish/v1/UpdateService": {"FirmwareInventory": Document{"@odata.id": "/redfish/v1/UpdateService/FirmwareInventory"}}, "/redfish/v1/Managers/BMC/NetworkProtocol": {"NTP": Document{"ProtocolEnabled": false, "NTPServers": []any{}}}},
		collections: map[string][]Resource{"/redfish/v1/Managers": {{URI: "/redfish/v1/Managers/BMC", Document: Document{"FirmwareVersion": "1.0", "DateTime": "2026-11-06T12:00:00Z", "NetworkProtocol": Document{"@odata.id": "/redfish/v1/Managers/BMC/NetworkProtocol"}}}}},
		failures:    map[string]error{},
	}, Document{"Managers": Document{"@odata.id": "/redfish/v1/Managers"}, "UpdateService": Document{"@odata.id": "/redfish/v1/UpdateService"}}, Resource{URI: "/redfish/v1/Systems/DPU", Document: Document{}}
}

var _ = Describe("optional Redfish software observations", func() {
	It("classifies component IDs and keeps raw versions and resource provenance", func() {
		for id, expected := range map[string]string{"BMC_Firmware": "bmc", "Bluefield_BMC_ERoT_FW": "bmc-erot", "Bluefield_CPU_ERoT_FW": "cpu-erot", "ERoT_Firmware": "erot", "DPU_OS": "dpu-os", "DPU_BSP": "dpu-bsp", "CEC_Firmware": "cec", "DPU_ATF": "atf", "DPU_UEFI": "uefi", "NIC_Firmware": "nic", "BMC_Golden_Image": "golden", "unrecognized": "other"} {
			f := firmwareRecord(Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/" + id, Document: Document{"Id": id, "Name": "Inventory entry", "Version": " v1.2 "}}, "", "", "https://bmc.invalid")
			Expect(f.Component).To(Equal(expected), id)
			Expect(f.RawID).To(Equal(id))
			Expect(f.RawVersion).To(Equal(" v1.2 "))
			Expect(f.EvidenceRefs).To(BeNil())
			Expect(f.Activation).To(Equal("unknown"))
			Expect(f.ActivationReasonCode).To(Equal("ACTIVATION_NOT_EXPOSED"))
			Expect(f.ActivationMessage).NotTo(BeEmpty())
		}
	})
	It("requires explicit target links for activation and does not infer it from names or enabled status", func() {
		doc := Document{"Id": "DPU_OS", "Name": "Active image", "Version": "1", "Status": Document{"State": "Enabled"}, "Updateable": true}
		record := Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/os", Document: doc}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", "https://bmc.invalid").Activation).To(Equal("unknown"))
		doc["Links"] = Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/other"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", "https://bmc.invalid").Activation).To(Equal("unknown"))
		doc["Links"] = Document{"StagedTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", "https://bmc.invalid").Activation).To(Equal("staged"))
		doc["Links"] = Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", "https://bmc.invalid").Activation).To(Equal("running"))
		Object(doc, "Links")["StagedTargets"] = []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}
		f := firmwareRecord(record, "/redfish/v1/Systems/selected", "", "https://bmc.invalid")
		Expect(f.Activation).To(Equal("unknown"))
		Expect(f.ActivationReasonCode).To(Equal("ACTIVATION_CONFLICT"))
	})
	It("normalizes validated same-origin activation targets and keeps staged versions separate", func() {
		reader, root, system := softwareFixture()
		device := inventory.Device{BMCEndpoint: "https://bmc.invalid"}
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/nic-current", Document: Document{"Id": "NIC_Firmware", "Version": "1.0", "Links": Document{"ActiveTargets": []any{Document{"@odata.id": "https://bmc.invalid/redfish/v1/Systems/DPU/"}}}}},
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/nic-next", Document: Document{"Id": "NIC_Firmware", "Version": "2.0", "Links": Document{"StagedTargets": []any{Document{"@odata.id": "https://bmc.invalid/redfish/v1/Systems/DPU"}}}}},
		}
		collectSoftware(context.Background(), reader, root, system, &device)
		count := 0
		for _, f := range device.Firmware {
			if f.Component != "nic" {
				continue
			}
			count++
			Expect(f.State).To(Equal("known"))
			Expect(f.ReasonCode).To(BeEmpty())
			Expect(f.ActivationReasonCode).To(BeEmpty())
			if f.RawVersion == "1.0" {
				Expect(f.Activation).To(Equal("running"))
			} else {
				Expect(f.Activation).To(Equal("staged"))
			}
		}
		Expect(count).To(Equal(2))
		Expect(device.Observations).NotTo(HaveKey("firmware.nic"))
		for _, ref := range []string{"/redfish/v1/Systems/DPU/", "https://bmc.invalid:443/redfish/v1/Systems/DPU", "https://BMC.INVALID/redfish/v1/Systems/DPU/"} {
			f := firmwareRecord(Resource{Document: Document{"Id": "NIC_Firmware", "Version": "1", "Links": Document{"ActiveTargets": []any{Document{"@odata.id": ref}}}}}, "/redfish/v1/Systems/DPU/", "", "https://bmc.invalid")
			Expect(f.Activation).To(Equal("running"), ref)
		}
		f := firmwareRecord(Resource{Document: Document{"Id": "BMC", "Version": "1", "Links": Document{"ActiveTargets": []any{Document{"@odata.id": "https://bmc.invalid/redfish/v1/Managers/BMC/"}}}}}, "/redfish/v1/Systems/DPU", "/redfish/v1/Managers/BMC/", "https://bmc.invalid")
		Expect(f.Activation).To(Equal("running"))
	})
	It("never promotes foreign, malformed or query-bearing activation links", func() {
		for _, scenario := range []struct {
			value any
			code  string
		}{
			{[]any{Document{"@odata.id": "https://foreign.invalid/redfish/v1/Systems/DPU"}}, "CROSS_ORIGIN_LINK"},
			{[]any{Document{"@odata.id": "//bmc.invalid/redfish/v1/Systems/DPU"}}, "CROSS_ORIGIN_LINK"},
			{[]any{Document{"@odata.id": "https://reader:secret@bmc.invalid/redfish/v1/Systems/DPU"}}, "UNSAFE_LINK"},
			{[]any{Document{"@odata.id": "/redfish/v1/Systems/DPU/../DPU"}}, "UNSAFE_LINK"},
			{[]any{Document{"@odata.id": "/redfish/v1/Systems/%44PU"}}, "UNSAFE_LINK"},
			{[]any{Document{"@odata.id": "/redfish/v1/Systems/DPU#fragment"}}, "UNSAFE_LINK"},
			{[]any{Document{"@odata.id": "/redfish/v1/Systems/DPU?$skip=0"}}, "UNSAFE_QUERY"},
			{[]any{Document{"@odata.id": "/redfish/v1/Systems/DPU?"}}, "UNSAFE_QUERY"},
			{[]any{Document{"@odata.id": 123}}, "UNSAFE_LINK"},
			{Document{"@odata.id": "/redfish/v1/Systems/DPU"}, "ACTIVATION_LINK_INVALID"},
		} {
			// A valid active member must not hide a malformed or foreign staged
			// member; otherwise incomplete activation evidence could pass a BOM.
			doc := Document{"Id": "NIC_Firmware", "Version": "1", "Links": Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/DPU"}}, "StagedTargets": scenario.value}}
			f := firmwareRecord(Resource{Document: doc}, "/redfish/v1/Systems/DPU", "", "https://bmc.invalid")
			Expect(f.Activation).To(Equal("unknown"))
			Expect(f.ActivationReasonCode).To(Equal(scenario.code))
			Expect(f.ActivationMessage).NotTo(BeEmpty())
			Expect(f.ActivationMessage).NotTo(ContainSubstring("secret"))
		}
		doc := Document{"Id": "NIC_Firmware", "Version": "1", "Links": Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/DPU"}, Document{"@odata.id": "https://foreign.invalid/redfish/v1/Systems/DPU"}}}}
		f := firmwareRecord(Resource{Document: doc}, "/redfish/v1/Systems/DPU", "", "https://bmc.invalid")
		Expect(f.Activation).To(Equal("unknown"))
		Expect(f.ActivationReasonCode).To(Equal("CROSS_ORIGIN_LINK"))
	})
	It("retains a denied source reason and does not equate it with an absent property", func() {
		reader, root, system := softwareFixture()
		reader.failures["/redfish/v1/UpdateService/FirmwareInventory"] = &Error{Code: "ACCESS_DENIED", Status: 403}
		reader.failures["/redfish/v1/Managers/BMC/NetworkProtocol"] = &Error{Code: "ACCESS_DENIED", Status: 403}
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d)
		Expect(d.Time.DateTime).To(Equal("2026-11-06T12:00:00Z"))
		Expect(d.Observations["bmc-ntp"].ReasonCode).To(Equal("ACCESS_DENIED"))
		Expect(d.Observations["bmc-ntp"].Message).To(ContainSubstring("403"))
		for _, f := range d.Firmware {
			if f.Component != "bmc" {
				Expect(f.ReasonCode).To(Equal("ACCESS_DENIED"))
				Expect(f.Message).NotTo(BeEmpty())
			}
		}
	})
	It("preserves available entries in an incomplete firmware collection", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{{URI: "/redfish/v1/UpdateService/FirmwareInventory/os", Document: Document{"Id": "DPU_OS", "Version": "24.04"}}}
		reader.failures["/redfish/v1/UpdateService/FirmwareInventory"] = &Error{Code: "ACCESS_DENIED", Status: 403}
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d)
		Expect(d.Observations["firmware-inventory"].State).To(Equal("unavailable"))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "dpu-os"), HaveField("RawVersion", "24.04"), HaveField("State", "known"))))
	})
	It("retains conflicting manager and component versions instead of selecting one", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{{URI: "/redfish/v1/UpdateService/FirmwareInventory/bmc", Document: Document{"Id": "BMC_Firmware", "Version": "2.0"}}}
		d := inventory.Device{}
		selected := collectSoftware(context.Background(), reader, root, system, &d)
		Expect(selected).To(BeEmpty())
		Expect(d.Observations["firmware.bmc"].ReasonCode).To(Equal("FIRMWARE_VERSION_CONFLICT"))
		count := 0
		for _, f := range d.Firmware {
			if f.Component == "bmc" {
				count++
				Expect(f.State).To(Equal("conflict"))
				Expect(f.RawVersion).NotTo(BeEmpty())
			}
		}
		Expect(count).To(Equal(2))
	})
	It("keeps BMC and CPU ERoT versions distinct without conflating unidentified roots of trust", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/BMC", Document: Document{"Id": "BMC_Firmware", "Version": "1.0"}},
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/BMC_ERoT", Document: Document{"Id": "Bluefield_BMC_ERoT_FW", "Name": "BMC Firmware", "Version": "2.0"}},
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/CPU_ERoT", Document: Document{"Id": "Bluefield_CPU_ERoT_FW", "Version": "3.0"}},
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/ERoT_1", Document: Document{"Id": "ERoT_1", "Version": "4.0"}},
			{URI: "/redfish/v1/UpdateService/FirmwareInventory/ERoT_2", Document: Document{"Id": "ERoT_2", "Version": "5.0"}},
		}
		d := inventory.Device{}
		Expect(collectSoftware(context.Background(), reader, root, system, &d)).To(Equal("1.0"))
		Expect(d.Observations).NotTo(HaveKey("firmware.bmc"))
		Expect(d.Firmware).NotTo(ContainElement(HaveField("State", "conflict")))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "bmc-erot"), HaveField("RawVersion", "2.0"), HaveField("State", "known"))))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "cpu-erot"), HaveField("RawVersion", "3.0"), HaveField("State", "known"))))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "erot"), HaveField("RawVersion", "4.0"), HaveField("State", "known"))))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "erot"), HaveField("RawVersion", "5.0"), HaveField("State", "known"))))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "cec"), HaveField("RawVersion", ""), HaveField("ReasonCode", "COMPONENT_NOT_EXPOSED"))))
	})
	It("keeps an explicitly staged successor separate from the manager version", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{{URI: "/redfish/v1/UpdateService/FirmwareInventory/bmc", Document: Document{"Id": "BMC_Firmware", "Version": "2.0", "Links": Document{"StagedTargets": []any{Document{"@odata.id": "/redfish/v1/Managers/BMC"}}}}}}
		d := inventory.Device{}
		Expect(collectSoftware(context.Background(), reader, root, system, &d)).To(Equal("1.0"))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "bmc"), HaveField("RawVersion", "2.0"), HaveField("Activation", "staged"))))
	})
	It("does not select the first of multiple managers and preserves explicit false NTP", func() {
		reader, root, system := softwareFixture()
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d)
		Expect(d.Time.NTPEnabled).NotTo(BeNil())
		Expect(*d.Time.NTPEnabled).To(BeFalse())
		Expect(d.Observations["bmc-ntp"].State).To(Equal("observed"))
		reader.collections["/redfish/v1/Managers"] = append(reader.collections["/redfish/v1/Managers"], Resource{URI: "/redfish/v1/Managers/other", Document: Document{}})
		d = inventory.Device{}
		Expect(collectSoftware(context.Background(), reader, root, system, &d)).To(BeEmpty())
		Expect(d.Time.ReasonCode).To(Equal("MANAGER_SELECTION_AMBIGUOUS"))
		Expect(d.Time.Message).NotTo(BeEmpty())
	})
	It("keeps missing links, missing versions, invalid clock and transport errors distinct", func() {
		reader, _, system := softwareFixture()
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, Document{}, system, &d)
		Expect(d.Observations["firmware-inventory"].ReasonCode).To(Equal("LINK_NOT_EXPOSED"))
		f := firmwareRecord(Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/os", Document: Document{"Id": "DPU_OS"}}, "", "", "https://bmc.invalid")
		Expect(f.ReasonCode).To(Equal("VERSION_NOT_EXPOSED"))
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/Managers"][0].Document["DateTime"] = "invalid"
		d = inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d)
		Expect(d.Time.ReasonCode).To(Equal("INVALID_DATETIME"))
		Expect(d.Time.DateTime).To(BeEmpty())
		for _, err := range []error{context.DeadlineExceeded, context.Canceled, &Error{Code: "RESOURCE_NOT_FOUND", Status: 404}, errors.New("secret-response")} {
			state := unavailable(err)
			Expect(state.ReasonCode).NotTo(BeEmpty())
			Expect(state.Message).NotTo(BeEmpty())
			Expect(state.Message).NotTo(ContainSubstring("secret-response"))
		}
	})
	It("returns already read members with a later collection error", func() {
		first := "/redfish/v1/UpdateService/FirmwareInventory/first"
		second := "/redfish/v1/UpdateService/FirmwareInventory/second"
		collection := "/redfish/v1/UpdateService/FirmwareInventory"
		c, closeServer := graphClient(map[string]Document{
			collection: {"Members@odata.count": 2, "Members": []any{Document{"@odata.id": first}, Document{"@odata.id": second}}},
			first:      {"Id": "DPU_OS", "Version": "24.04"},
		}, nil)
		defer closeServer()
		records, err := c.Collection(context.Background(), collection)
		Expect(Code(err)).To(Equal("RESOURCE_NOT_FOUND"))
		Expect(records).To(HaveLen(1))
		Expect(records[0].URI).To(Equal(first))
	})
	It("distinguishes incomplete response bodies from response size limits", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		}))
		defer server.Close()
		c := &Client{origin: server.URL, http: server.Client()}
		_, err := c.Get(context.Background(), "/redfish/v1/Managers")
		Expect(Code(err)).To(Equal("RESPONSE_READ_FAILED"))
	})
	It("classifies an HTTP client timeout separately from other transport errors", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(50 * time.Millisecond); w.WriteHeader(200) }))
		defer server.Close()
		client := server.Client()
		client.Timeout = time.Millisecond
		c := &Client{origin: server.URL, http: client}
		_, err := c.Get(context.Background(), "/redfish/v1/Managers")
		Expect(Code(err)).To(Equal("BMC_TIMEOUT"))
	})
})
