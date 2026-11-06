// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func hostGraph() map[string]Document {
	link := func(uri string) Document { return Document{"@odata.id": uri} }
	collection := func(uris ...string) Document {
		members := []any{}
		for _, uri := range uris {
			members = append(members, link(uri))
		}
		return Document{"Members@odata.count": len(uris), "Members": members}
	}
	pci := func(id string, bus int, slot, name string) Document {
		return Document{"ClassCode": 2, "DeviceType": "NIC", "DeviceResourceId": id, "SegmentNumber": 0, "BusNumber": bus, "DeviceNumber": 0, "FunctionNumber": 0, "VendorID": 5555, "DeviceID": 41695, "LocationString": slot, "Name": name}
	}
	return map[string]Document{
		"/redfish/v1/Systems/host":                               {"NetworkInterfaces": link("/redfish/v1/Systems/host/NetworkInterfaces"), "Links": Document{"Chassis": []any{link("/redfish/v1/Chassis/enclosure")}}, "Oem": Document{"Hpe": Document{"Links": Document{"PCIDevices": []any{link("/redfish/v1/Systems/host/PCIDevices")}}}}},
		"/redfish/v1/Systems/host/NetworkInterfaces":             collection("/redfish/v1/Systems/host/NetworkInterfaces/embedded"),
		"/redfish/v1/Systems/host/NetworkInterfaces/embedded":    {"Links": Document{"NetworkAdapter": link("/redfish/v1/Chassis/enclosure/NetworkAdapters/embedded/")}},
		"/redfish/v1/Chassis/enclosure":                          {"NetworkAdapters": link("/redfish/v1/Chassis/enclosure/NetworkAdapters")},
		"/redfish/v1/Chassis/enclosure/NetworkAdapters":          collection("/redfish/v1/Chassis/enclosure/NetworkAdapters/embedded", "/redfish/v1/Chassis/enclosure/NetworkAdapters/dpu"),
		"/redfish/v1/Chassis/enclosure/NetworkAdapters/embedded": {"Id": "embedded", "Model": "Embedded NIC", "SerialNumber": "NIC-A"},
		"/redfish/v1/Chassis/enclosure/NetworkAdapters/dpu":      {"Id": "dpu", "Model": "BlueField-4", "SerialNumber": "DPU-A"},
		"/redfish/v1/Systems/host/PCIDevices":                    collection("/redfish/v1/Systems/host/PCIDevices/dpu", "/redfish/v1/Systems/host/PCIDevices/other"),
		"/redfish/v1/Systems/host/PCIDevices/dpu":                pci("dpu", 129, "Slot 5", "BlueField-4"),
		"/redfish/v1/Systems/host/PCIDevices/other":              pci("", 33, "Slot 1", "ConnectX"),
	}
}

func graphClient(docs map[string]Document, mutate func(string, int, Document)) (*Client, func()) {
	reads := map[string]int{}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimSuffix(r.URL.Path, "/")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		doc, ok := docs[uri]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reads[uri]++
		if mutate != nil {
			mutate(uri, reads[uri], doc)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	}))
	return &Client{origin: s.URL, http: s.Client()}, s.Close
}

var _ = Describe("host inventory ownership and partial identity", func() {
	It("joins HPE chassis cards to System PCI records and retains cards without serials", func() {
		c, closeServer := graphClient(hostGraph(), nil)
		defer closeServer()
		system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
		Expect(err).NotTo(HaveOccurred())
		cards, err := collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, "host-a", true, func(uri, prop string) string { return uri + "#" + prop })
		Expect(err).NotTo(HaveOccurred())
		Expect(cards).To(HaveLen(3))
		Expect(cards[0].SerialNumber).To(Equal("DPU-A"))
		Expect(cards[0].PCIAddress).To(Equal("0000:81:00.0"))
		Expect(cards[0].Slot).To(Equal("Slot 5"))
		Expect(cards[0].VendorID).To(Equal("15b3"))
		Expect(cards[0].DeviceID).To(Equal("a2df"))
		Expect(cards[0].IdentityState).To(Equal("known"))
		Expect(cards[0].EvidenceRefs).To(ContainElement("/redfish/v1/Systems/host/PCIDevices/dpu#DeviceResourceId"))
		Expect(cards[2].SerialNumber).To(BeEmpty())
		Expect(cards[2].ReasonCode).To(Equal("CARD_SERIAL_MISSING"))
		Expect(cards[2].IdentityState).To(Equal("unknown"))
		Expect(cards[2].PCIAddress).To(Equal("0000:21:00.0"))
		Expect(cards[2].Key).NotTo(BeEmpty())
	})
	It("keeps the host resource observation key across replacement and newly exposed identity", func() {
		keys := []string{}
		for _, serial := range []string{"DPU-A", "DPU-REPLACED", ""} {
			docs := hostGraph()
			docs["/redfish/v1/Chassis/enclosure/NetworkAdapters/dpu"]["SerialNumber"] = serial
			c, closeServer := graphClient(docs, nil)
			system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
			Expect(err).NotTo(HaveOccurred())
			cards, err := collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, "host-a", true, func(uri, prop string) string { return uri + "#" + prop })
			closeServer()
			Expect(err).NotTo(HaveOccurred())
			found := false
			for _, card := range cards {
				if card.Slot != "Slot 5" {
					continue
				}
				found = true
				Expect(card.SerialNumber).To(Equal(serial))
				keys = append(keys, card.Key)
			}
			Expect(found).To(BeTrue())
		}
		Expect(keys).To(HaveLen(3))
		Expect(keys[1]).To(Equal(keys[0]))
		Expect(keys[2]).To(Equal(keys[0]))
	})
	It("keeps resource observation keys independent of collection order and bound to the host", func() {
		keys := []string{}
		for _, hostID := range []string{"host-a", "host-a", "host-b"} {
			docs := hostGraph()
			if len(keys) > 0 {
				members := docs["/redfish/v1/Chassis/enclosure/NetworkAdapters"]["Members"].([]any)
				members[0], members[1] = members[1], members[0]
			}
			c, closeServer := graphClient(docs, nil)
			system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
			Expect(err).NotTo(HaveOccurred())
			cards, err := collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, hostID, true, func(uri, prop string) string { return uri + "#" + prop })
			closeServer()
			Expect(err).NotTo(HaveOccurred())
			for _, card := range cards {
				if card.SerialNumber == "DPU-A" {
					keys = append(keys, card.Key)
				}
			}
		}
		Expect(keys).To(HaveLen(3))
		Expect(keys[1]).To(Equal(keys[0]))
		Expect(keys[2]).NotTo(Equal(keys[0]))
	})
	It("does not assign an uncorrelated shared-chassis adapter to the selected host", func() {
		docs := hostGraph()
		docs["/redfish/v1/Systems/host/PCIDevices/dpu"]["DeviceResourceId"] = "not-a-chassis-card"
		c, closeServer := graphClient(docs, nil)
		defer closeServer()
		system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
		Expect(err).NotTo(HaveOccurred())
		cards, err := collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, "host-a", true, func(uri, prop string) string { return uri + "#" + prop })
		Expect(err).NotTo(HaveOccurred())
		for _, card := range cards {
			Expect(card.SerialNumber).NotTo(Equal("DPU-A"))
		}
	})
	It("rejects duplicate physical serials and ambiguous PCI-to-adapter joins", func() {
		for _, duplicateID := range []bool{false, true} {
			docs := hostGraph()
			if duplicateID {
				docs["/redfish/v1/Chassis/enclosure/NetworkAdapters/embedded"]["Id"] = "dpu"
			} else {
				docs["/redfish/v1/Chassis/enclosure/NetworkAdapters/embedded"]["SerialNumber"] = "DPU-A"
			}
			c, closeServer := graphClient(docs, nil)
			system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
			Expect(err).NotTo(HaveOccurred())
			_, err = collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, "host-a", true, func(uri, prop string) string { return uri + "#" + prop })
			Expect(err).To(HaveOccurred())
			if duplicateID {
				Expect(Code(err)).To(Equal("ADAPTER_PCI_AMBIGUOUS"))
			} else {
				Expect(Code(err)).To(Equal("CARD_IDENTITY_AMBIGUOUS"))
			}
			closeServer()
		}
	})
	It("detects card replacement during collection", func() {
		c, closeServer := graphClient(hostGraph(), func(uri string, reads int, doc Document) {
			if uri == "/redfish/v1/Chassis/enclosure/NetworkAdapters/dpu" && reads > 1 {
				doc["SerialNumber"] = "DPU-REPLACED"
			}
		})
		defer closeServer()
		system, err := c.Get(context.Background(), "/redfish/v1/Systems/host")
		Expect(err).NotTo(HaveOccurred())
		_, err = collectHostAdapters(context.Background(), c, Resource{URI: "/redfish/v1/Systems/host", Document: system}, "host-a", true, func(uri, prop string) string { return uri + "#" + prop })
		Expect(Code(err)).To(Equal("IDENTITY_CHANGED"))
	})
	It("does not guess a PCI domain or address when components are unavailable", func() {
		Expect(hpePCIAddress(Document{"BusNumber": float64(42)})).To(BeEmpty())
		Expect(hexID(Document{"VendorID": "15b3"}, "VendorID")).To(BeEmpty())
	})
})
