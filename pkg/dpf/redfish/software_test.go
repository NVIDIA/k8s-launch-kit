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
func softwareEvidence(uri, property string) string { return uri + "#" + property }

var _ = Describe("optional Redfish software observations", func() {
	It("classifies component IDs and keeps raw versions and resource provenance", func() {
		for id, expected := range map[string]string{"BMC_Firmware": "bmc", "DPU_OS": "dpu-os", "DPU_BSP": "dpu-bsp", "CEC_Firmware": "cec", "DPU_ATF": "atf", "DPU_UEFI": "uefi", "NIC_Firmware": "nic", "BMC_Golden_Image": "golden", "unrecognized": "other"} {
			f := firmwareRecord(Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/" + id, Document: Document{"Id": id, "Name": "Inventory entry", "Version": " v1.2 "}}, "", "", softwareEvidence)
			Expect(f.Component).To(Equal(expected), id)
			Expect(f.RawID).To(Equal(id))
			Expect(f.RawVersion).To(Equal(" v1.2 "))
			Expect(f.EvidenceRefs).To(ContainElement(f.ResourceURI + "#Id,Name,Version"))
			Expect(f.Activation).To(Equal("unknown"))
			Expect(f.ActivationReasonCode).To(Equal("ACTIVATION_NOT_EXPOSED"))
			Expect(f.ActivationMessage).NotTo(BeEmpty())
		}
	})
	It("requires explicit target links for activation and does not infer it from names or enabled status", func() {
		doc := Document{"Id": "DPU_OS", "Name": "Active image", "Version": "1", "Status": Document{"State": "Enabled"}, "Updateable": true}
		record := Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/os", Document: doc}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", softwareEvidence).Activation).To(Equal("unknown"))
		doc["Links"] = Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/other"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", softwareEvidence).Activation).To(Equal("unknown"))
		doc["Links"] = Document{"StagedTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", softwareEvidence).Activation).To(Equal("staged"))
		doc["Links"] = Document{"ActiveTargets": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}
		Expect(firmwareRecord(record, "/redfish/v1/Systems/selected", "", softwareEvidence).Activation).To(Equal("running"))
		Object(doc, "Links")["StagedTargets"] = []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}
		f := firmwareRecord(record, "/redfish/v1/Systems/selected", "", softwareEvidence)
		Expect(f.Activation).To(Equal("unknown"))
		Expect(f.ActivationReasonCode).To(Equal("ACTIVATION_CONFLICT"))
	})
	It("retains a denied source reason and does not equate it with an absent property", func() {
		reader, root, system := softwareFixture()
		reader.failures["/redfish/v1/UpdateService/FirmwareInventory"] = &Error{Code: "ACCESS_DENIED", Status: 403}
		reader.failures["/redfish/v1/Managers/BMC/NetworkProtocol"] = &Error{Code: "ACCESS_DENIED", Status: 403}
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)
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
		collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)
		Expect(d.Observations["firmware-inventory"].State).To(Equal("unavailable"))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "dpu-os"), HaveField("RawVersion", "24.04"), HaveField("State", "known"))))
	})
	It("retains conflicting manager and component versions instead of selecting one", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{{URI: "/redfish/v1/UpdateService/FirmwareInventory/bmc", Document: Document{"Id": "BMC_Firmware", "Version": "2.0"}}}
		d := inventory.Device{}
		selected := collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)
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
	It("keeps an explicitly staged successor separate from the manager version", func() {
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/UpdateService/FirmwareInventory"] = []Resource{{URI: "/redfish/v1/UpdateService/FirmwareInventory/bmc", Document: Document{"Id": "BMC_Firmware", "Version": "2.0", "Links": Document{"StagedTargets": []any{Document{"@odata.id": "/redfish/v1/Managers/BMC"}}}}}}
		d := inventory.Device{}
		Expect(collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)).To(Equal("1.0"))
		Expect(d.Firmware).To(ContainElement(And(HaveField("Component", "bmc"), HaveField("RawVersion", "2.0"), HaveField("Activation", "staged"))))
	})
	It("does not select the first of multiple managers and preserves explicit false NTP", func() {
		reader, root, system := softwareFixture()
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)
		Expect(d.Time.NTPEnabled).NotTo(BeNil())
		Expect(*d.Time.NTPEnabled).To(BeFalse())
		Expect(d.Observations["bmc-ntp"].State).To(Equal("observed"))
		reader.collections["/redfish/v1/Managers"] = append(reader.collections["/redfish/v1/Managers"], Resource{URI: "/redfish/v1/Managers/other", Document: Document{}})
		d = inventory.Device{}
		Expect(collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)).To(BeEmpty())
		Expect(d.Time.ReasonCode).To(Equal("MANAGER_SELECTION_AMBIGUOUS"))
		Expect(d.Time.Message).NotTo(BeEmpty())
	})
	It("keeps missing links, missing versions, invalid clock and transport errors distinct", func() {
		reader, _, system := softwareFixture()
		d := inventory.Device{}
		collectSoftware(context.Background(), reader, Document{}, system, &d, softwareEvidence)
		Expect(d.Observations["firmware-inventory"].ReasonCode).To(Equal("LINK_NOT_EXPOSED"))
		f := firmwareRecord(Resource{URI: "/redfish/v1/UpdateService/FirmwareInventory/os", Document: Document{"Id": "DPU_OS"}}, "", "", softwareEvidence)
		Expect(f.ReasonCode).To(Equal("VERSION_NOT_EXPOSED"))
		reader, root, system := softwareFixture()
		reader.collections["/redfish/v1/Managers"][0].Document["DateTime"] = "invalid"
		d = inventory.Device{}
		collectSoftware(context.Background(), reader, root, system, &d, softwareEvidence)
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
