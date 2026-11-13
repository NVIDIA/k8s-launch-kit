// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BF4 B4240 physical identity", func() {
	It("accepts the two explicit firmware graphs and rejects unsupported or conflicting identity", func() {
		Expect(os.Setenv("DPF_BF4_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_BF4_PASSWORD")
		for _, scenario := range []struct {
			name   string
			legacy bool
			code   string
		}{
			{"physical card", false, ""}, {"legacy component", true, ""}, {"nullable legacy metadata", true, ""},
			{"ambiguous cards", false, "CARD_IDENTITY_AMBIGUOUS"}, {"wrong serial", false, "CARD_IDENTITY_AMBIGUOUS"},
			{"wrong model", false, "CARD_IDENTITY_AMBIGUOUS"}, {"wrong manufacturer", false, "CARD_IDENTITY_AMBIGUOUS"},
			{"foreign system link", false, "CARD_IDENTITY_AMBIGUOUS"}, {"foreign containment", true, "CARD_IDENTITY_AMBIGUOUS"},
			{"missing containment", true, "CARD_IDENTITY_AMBIGUOUS"}, {"wrong system chassis", true, "CARD_IDENTITY_AMBIGUOUS"},
			{"graph changes", true, "IDENTITY_CHANGED"}, {"malformed system model", true, "DPU_IDENTITY_CONFLICT"}, {"malformed system serial", true, "DPU_IDENTITY_CONFLICT"},
			{"conflicting parent model", true, "CARD_IDENTITY_AMBIGUOUS"}, {"conflicting parent maker", true, "CARD_IDENTITY_AMBIGUOUS"}, {"malformed parent serial", true, "CARD_IDENTITY_AMBIGUOUS"}, {"malformed parent model", true, "CARD_IDENTITY_AMBIGUOUS"}, {"nullable parent metadata", true, ""}, {"conflicting parent serial", true, "CARD_IDENTITY_AMBIGUOUS"}, {"malformed parent maker", true, "CARD_IDENTITY_AMBIGUOUS"},
		} {
			By(scenario.name)
			link := func(uri string) Document { return Document{"@odata.id": uri} }
			boardReads := 0
			writes := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes = append(writes, r.Method+" "+r.URL.Path)
				}
				var doc Document
				switch r.URL.Path {
				case "/redfish/v1/":
					product := "BlueField-4"
					if scenario.legacy {
						product = "B4240"
					}
					doc = Document{"Vendor": "Nvidia", "Product": product, "Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}, "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis")}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "fixture-token")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
					w.WriteHeader(http.StatusCreated)
					doc = Document{}
				case "/redfish/v1/SessionService/Sessions/owned":
					if r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					doc = Document{"UserName": "reader"}
				case "/redfish/v1/Systems":
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/DPU")}}
				case "/redfish/v1/Systems/DPU":
					doc = Document{"Manufacturer": "Nvidia", "Model": "BlueField-4", "SerialNumber": "CARD-A", "UUID": "11111111-1111-4111-8111-111111111111", "Links": Document{"Chassis": []any{link("/redfish/v1/Chassis/BMC")}}}
					if scenario.legacy {
						delete(doc, "Manufacturer")
						delete(doc, "Model")
						delete(doc, "SerialNumber")
						delete(doc, "UUID")
					}
					if scenario.name == "nullable legacy metadata" {
						doc["Manufacturer"] = nil
						doc["Model"] = nil
						doc["SerialNumber"] = nil
						doc["UUID"] = nil
					}
					if scenario.name == "malformed system model" {
						doc["Model"] = false
					}
					if scenario.name == "malformed system serial" {
						doc["SerialNumber"] = 123
					}
					if scenario.name == "wrong system chassis" {
						doc["Links"] = Document{"Chassis": []any{link("https://foreign.invalid/redfish/v1/Chassis/BMC")}}
					}
				case "/redfish/v1/Chassis":
					members := []any{link("/redfish/v1/Chassis/Board"), link("/redfish/v1/Chassis/BMC")}
					if scenario.name == "ambiguous cards" {
						members = append(members, link("/redfish/v1/Chassis/Other"))
					}
					doc = Document{"Members@odata.count": len(members), "Members": members}
				case "/redfish/v1/Chassis/Board", "/redfish/v1/Chassis/Other":
					doc = Document{"ChassisType": "Card", "Manufacturer": "Nvidia", "Model": "B4240", "SerialNumber": "CARD-A", "Links": Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}, "Contains": []any{link("/redfish/v1/Chassis/BMC")}}}
					if scenario.legacy {
						delete(doc, "Manufacturer")
						delete(doc, "Model")
						delete(doc, "SerialNumber")
					}
					if scenario.name == "conflicting parent serial" {
						doc["SerialNumber"] = "CARD-B"
					}
					if scenario.name == "malformed parent maker" {
						doc["Manufacturer"] = false
					}
					if scenario.name == "conflicting parent model" {
						doc["Model"] = "ConnectX-8"
					}
					if scenario.name == "conflicting parent maker" {
						doc["Manufacturer"] = "Other"
					}
					if scenario.name == "malformed parent serial" {
						doc["SerialNumber"] = 123
					}
					if scenario.name == "malformed parent model" {
						doc["Model"] = true
					}
					if scenario.name == "nullable parent metadata" {
						doc["Manufacturer"] = nil
						doc["Model"] = nil
						doc["SerialNumber"] = nil
					}
					if scenario.name == "missing containment" {
						Object(doc, "Links")["Contains"] = []any{}
					}
					if scenario.name == "wrong serial" {
						doc["SerialNumber"] = "CARD-B"
					}
					if scenario.name == "wrong model" {
						doc["Model"] = "B4240X"
						doc["Description"] = "NVIDIA BlueField-4 B4240 DPU"
					}
					if scenario.name == "wrong manufacturer" {
						doc["Manufacturer"] = "Other"
					}
					if scenario.name == "foreign system link" {
						Object(doc, "Links")["ComputerSystems"] = []any{link("https://foreign.invalid/redfish/v1/Systems/DPU")}
					}
				case "/redfish/v1/Chassis/BMC":
					boardReads++
					model := "BlueField-4"
					if scenario.legacy {
						model = "B4240"
					}
					doc = Document{"ChassisType": "Component", "Manufacturer": "Nvidia", "Model": model, "SerialNumber": "CARD-A", "Links": Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}}
					if scenario.legacy {
						Object(doc, "Links")["ContainedBy"] = link("/redfish/v1/Chassis/Board")
					}
					if scenario.name == "foreign containment" {
						Object(doc, "Links")["ContainedBy"] = link("https://foreign.invalid/redfish/v1/Chassis/Board")
					}
					if scenario.name == "graph changes" && boardReads > 1 {
						Object(doc, "Links")["ContainedBy"] = link("/redfish/v1/Chassis/Other")
					}
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				json.NewEncoder(w).Encode(doc)
			}))
			seed := fmt.Sprintf("credentials:\n  read: {username: reader, passwordRef: {env: DPF_BF4_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n  probes: {mode: inventory}\n", server.URL)
			input, err := config.Decode([]byte(seed), filepath.Join(GinkgoT().TempDir(), "site.yaml"))
			Expect(err).NotTo(HaveOccurred())
			req, err := config.Resolve(input)
			Expect(err).NotTo(HaveOccurred())
			d, evidence, err := (Collector{}).Collect(context.Background(), req.Targets[0], time.Second)
			if scenario.code == "" {
				Expect(err).NotTo(HaveOccurred())
				Expect(d.ObservationState).To(Equal("observed"))
				Expect(d.SerialNumber).To(Equal("CARD-A"))
				Expect(d.Model).To(Equal("B4240"))
				Expect(evidence).NotTo(BeEmpty())
				if scenario.legacy {
					Expect(d.SystemUUID).To(BeEmpty())
					Expect(d.Manufacturer).To(BeEmpty())
				}
			} else {
				Expect(Code(err)).To(Equal(scenario.code))
				Expect(d.ObservationState).NotTo(Equal("observed"))
			}
			Expect(writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
			server.Close()
		}
	})
})
