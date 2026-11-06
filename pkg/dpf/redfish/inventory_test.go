// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"
)

var _ = Describe("physical card identity", func() {
	It("records the linked card and rejects a replacement at the same URI", func() {
		for _, scenario := range []struct{ replace, absolute bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			replace := scenario.replace
			cardReads := 0
			writes := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes = append(writes, r.Method+" "+r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				var doc any
				switch r.URL.Path {
				case "/redfish/v1/":
					doc = Document{"SessionService": Document{"@odata.id": "/redfish/v1/SessionService"}, "Systems": Document{"@odata.id": "/redfish/v1/Systems"}, "Chassis": Document{"@odata.id": "/redfish/v1/Chassis"}}
				case "/redfish/v1/SessionService":
					doc = Document{"Sessions": Document{"@odata.id": "/redfish/v1/SessionService/Sessions"}}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "private-session")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
					w.WriteHeader(201)
					doc = Document{}
				case "/redfish/v1/SessionService/Sessions/owned":
					w.WriteHeader(204)
					return
				case "/redfish/v1/Systems":
					doc = Document{"Members@odata.count": 1, "Members": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}
				case "/redfish/v1/Systems/selected":
					doc = Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "NVIDIA", "Model": "BlueField-3"}
				case "/redfish/v1/Chassis":
					doc = Document{"Members@odata.count": 1, "Members": []any{Document{"@odata.id": "/redfish/v1/Chassis/Card"}}}
				case "/redfish/v1/Chassis/Card":
					cardReads++
					serial := "CARD-A"
					if replace && cardReads > 1 {
						serial = "CARD-B"
					}
					doc = Document{"Model": "BlueField-3", "SerialNumber": serial, "Links": Document{"ComputerSystems": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}}
				default:
					w.WriteHeader(404)
					return
				}
				if scenario.absolute {
					absoluteLinks(doc, "https://"+r.Host)
				}
				json.NewEncoder(w).Encode(doc)
			}))
			dir, e := os.MkdirTemp("", "dpf-card-")
			Expect(e).NotTo(HaveOccurred())
			ca := filepath.Join(dir, "ca.pem")
			Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)).To(Succeed())
			Expect(os.Setenv("DPF_CARD_PASSWORD", "private-password")).To(Succeed())
			source := fmt.Sprintf("management:\n  kubeconfigRef: {file: unused}\ncredentials:\n  read: {username: reader, passwordRef: {env: DPF_CARD_PASSWORD}}\ndiscovery:\n  tls: {caFile: '%s'}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read, systemURI: /redfish/v1/Systems/selected}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n", ca, server.URL)
			in, e := config.Decode([]byte(source), filepath.Join(dir, "site.yaml"))
			Expect(e).NotTo(HaveOccurred())
			req, e := config.Resolve(in)
			Expect(e).NotTo(HaveOccurred())
			device, evidence, e := (Collector{}).Collect(context.Background(), req.Targets[0], time.Second)
			if replace {
				Expect(e).To(HaveOccurred())
				Expect(Code(e)).To(Equal("IDENTITY_CHANGED"))
			} else {
				Expect(e).NotTo(HaveOccurred())
				Expect(device.SerialNumber).To(Equal("CARD-A"))
				Expect(device.ObservationState).To(Equal("observed"))
				Expect(device.Qualification).To(Equal("unqualified"))
				Expect(evidence).NotTo(BeEmpty())
			}
			Expect(writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
			server.Close()
			os.RemoveAll(dir)
			os.Unsetenv("DPF_CARD_PASSWORD")
		}
	})
	It("uses ComputerSystem linkage rather than the first chassis", func() {
		card := Document{"Links": Document{"ComputerSystems": []any{Document{"@odata.id": "/redfish/v1/Systems/selected"}}}}
		Expect((&Client{origin: "https://bmc.invalid"}).linkedToSystem(card, "/redfish/v1/Systems/selected")).To(BeTrue())
		Expect((&Client{origin: "https://bmc.invalid"}).linkedToSystem(card, "/redfish/v1/Systems/other")).To(BeFalse())
		card["Links"] = Document{"ComputerSystems": []any{Document{"@odata.id": "https://foreign.invalid/redfish/v1/Systems/selected"}}}
		Expect((&Client{origin: "https://bmc.invalid"}).linkedToSystem(card, "/redfish/v1/Systems/selected")).To(BeFalse())
	})
})

var _ = Describe("host UUID syntax", func() {
	It("rejects placeholders before assigning identity", func() {
		for _, value := range []string{"unknown", "default", "00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "bad-uuid"} {
			Expect(validHostUUID(value)).To(BeFalse())
		}
		Expect(validHostUUID("11111111-1111-4111-8111-111111111111")).To(BeTrue())
	})
})

func absoluteLinks(value any, origin string) {
	switch v := value.(type) {
	case Document:
		for key, item := range v {
			if key == "@odata.id" {
				if s, ok := item.(string); ok {
					v[key] = origin + s
				}
			} else {
				absoluteLinks(item, origin)
			}
		}
	case []any:
		for _, item := range v {
			absoluteLinks(item, origin)
		}
	}
}
