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
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("host BMC qualification metadata", func() {
	It("reads manager firmware only for applicable catalog entries without collecting host software or time", func() {
		for _, scenario := range []string{"no-catalog", "other-model", "no-verifier", "no-version-requirement", "qualified", "wrong-version", "denied", "ambiguous", "verification-failed"} {
			docs := hostGraph()
			docs["/redfish/v1/Systems/host"]["Manufacturer"] = "HPE"
			docs["/redfish/v1/Systems/host"]["Model"] = "Host"
			docs["/redfish/v1/Systems/host"]["UUID"] = "11111111-1111-4111-8111-111111111111"
			docs["/redfish/v1/Systems/host"]["Actions"] = Document{"#ComputerSystem.Reset": Document{"ResetType@Redfish.AllowableValues": []any{"On", "ForceOff"}}}
			link := func(uri string) Document { return Document{"@odata.id": uri} }
			docs["/redfish/v1/Managers"] = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Managers/BMC")}}
			docs["/redfish/v1/Managers/BMC"] = Document{"FirmwareVersion": "1.0", "DateTime": "2026-11-13T12:00:00Z", "NetworkProtocol": link("/redfish/v1/Managers/BMC/NetworkProtocol")}
			if scenario == "ambiguous" {
				docs["/redfish/v1/Managers"]["Members@odata.count"] = 2
				docs["/redfish/v1/Managers"]["Members"] = []any{link("/redfish/v1/Managers/BMC"), link("/redfish/v1/Managers/other")}
				docs["/redfish/v1/Managers/other"] = Document{"FirmwareVersion": "1.0"}
			}
			reads := map[string]int{}
			var mu sync.Mutex
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uri := strings.TrimSuffix(r.URL.Path, "/")
				mu.Lock()
				reads[uri]++
				mu.Unlock()
				var doc Document
				switch uri {
				case "/redfish/v1":
					doc = Document{"Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}, "Systems": link("/redfish/v1/Systems"), "Managers": link("/redfish/v1/Managers"), "UpdateService": link("/redfish/v1/UpdateService")}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "fixture")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
					w.WriteHeader(http.StatusCreated)
					doc = Document{}
				case "/redfish/v1/SessionService/Sessions/owned":
					w.WriteHeader(http.StatusNoContent)
					return
				case "/redfish/v1/Systems":
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/host")}}
				default:
					if scenario == "denied" && strings.HasPrefix(uri, "/redfish/v1/Managers") {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					var ok bool
					doc, ok = docs[uri]
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(doc)
			}))
			DeferCleanup(server.Close)
			Expect(os.Setenv("DPF_HOST_QUALIFICATION_PASSWORD", "fixture")).To(Succeed())
			DeferCleanup(os.Unsetenv, "DPF_HOST_QUALIFICATION_PASSWORD")
			source := fmt.Sprintf("credentials:\n  read: {username: reader, passwordRef: {env: DPF_HOST_QUALIFICATION_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{bmcEndpoint: '%s', credentialRef: read}]\n  probes: {mode: inventory}\n", server.URL)
			in, err := config.Decode([]byte(source), filepath.Join(GinkgoT().TempDir(), "site.yaml"))
			Expect(err).NotTo(HaveOccurred())
			req, err := config.Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			verified := 0
			qualification := Qualification{Adapter: "hpe-standard-v1", Model: "Host", BMCFirmware: "1.0", VerifyCompleteness: func(context.Context, Reader, Resource) error {
				verified++
				if scenario == "verification-failed" {
					return &Error{Code: "INCOMPLETE_COLLECTION"}
				}
				return nil
			}}
			switch scenario {
			case "other-model":
				qualification.Model = "Other"
			case "no-verifier":
				qualification.VerifyCompleteness = nil
			case "no-version-requirement":
				qualification.BMCFirmware = ""
			case "wrong-version":
				qualification.BMCFirmware = "2.0"
			}
			collector := Collector{Qualifications: []Qualification{qualification}}
			if scenario == "no-catalog" {
				collector.Qualifications = nil
			}
			device, _, err := collector.Collect(context.Background(), req.Targets[0], time.Second)
			Expect(err).NotTo(HaveOccurred(), scenario)
			Expect(device.Firmware).To(BeEmpty(), scenario)
			Expect(device.Time).To(BeZero(), scenario)
			Expect(device.Observations).To(BeEmpty(), scenario)
			Expect(device.AdvertisedResetTypes).To(Equal([]string{"On", "ForceOff"}), scenario)
			Expect(device.Adapters).To(ContainElement(And(HaveField("SerialNumber", "DPU-A"), HaveField("PCIAddress", "0000:81:00.0"))), scenario)
			if scenario == "qualified" {
				Expect(device.Qualification).To(Equal("qualified"))
				Expect(device.InventoryCompleteness).To(Equal("complete"))
			} else {
				Expect(device.Qualification).To(Equal("unqualified"), scenario)
			}
			if scenario == "qualified" || scenario == "verification-failed" {
				Expect(verified).To(Equal(1), scenario)
			} else {
				Expect(verified).To(BeZero(), scenario)
			}
			mu.Lock()
			Expect(reads["/redfish/v1/UpdateService"]).To(BeZero(), scenario)
			Expect(reads["/redfish/v1/Managers/BMC/NetworkProtocol"]).To(BeZero(), scenario)
			Expect(reads["/redfish/v1/Systems/host"]).To(BeNumerically(">=", 3), scenario)
			if scenario == "no-catalog" || scenario == "other-model" || scenario == "no-verifier" || scenario == "no-version-requirement" {
				Expect(reads["/redfish/v1/Managers"]).To(BeZero(), scenario)
			} else {
				Expect(reads["/redfish/v1/Managers"]).To(BeNumerically(">", 0), scenario)
			}
			mu.Unlock()
		}
	})
})
