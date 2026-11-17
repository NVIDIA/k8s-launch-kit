// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

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
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Redfish collector to software BOM contract", func() {
	It("uses actual running activation and rejects staged, conflicting, partial and replaced observations", func() {
		Expect(os.Setenv("DPF_SOFTWARE_TEST_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_SOFTWARE_TEST_PASSWORD")
		dir, err := os.MkdirTemp("", "dpf-software-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		for _, scenario := range []struct {
			name, activation, expected string
			partial, replace           bool
		}{
			{name: "running", activation: "ActiveTargets", expected: "SOFTWARE_MINIMUM_MET"},
			{name: "staged", activation: "StagedTargets", expected: "SOFTWARE_VERSION_UNAVAILABLE"},
			{name: "conflicting activation", activation: "both", expected: "SOFTWARE_VERSION_CONFLICT"},
			{name: "partial collection", activation: "ActiveTargets", partial: true, expected: "ACCESS_DENIED"},
			{name: "identity replaced", activation: "ActiveTargets", replace: true, expected: "SOFTWARE_IDENTITY_UNAVAILABLE"},
		} {
			cardReads := 0
			writes := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes = append(writes, r.Method+" "+r.URL.Path)
				}
				link := func(uri string) redfish.Document { return redfish.Document{"@odata.id": uri} }
				var doc redfish.Document
				switch r.URL.Path {
				case "/redfish/v1/":
					doc = redfish.Document{"SessionService": link("/redfish/v1/SessionService"), "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "UpdateService": link("/redfish/v1/UpdateService")}
				case "/redfish/v1/SessionService":
					doc = redfish.Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "fixture-token")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
					w.WriteHeader(http.StatusCreated)
					doc = redfish.Document{}
				case "/redfish/v1/SessionService/Sessions/owned":
					w.WriteHeader(http.StatusNoContent)
					return
				case "/redfish/v1/Systems":
					doc = redfish.Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/DPU")}}
				case "/redfish/v1/Systems/DPU":
					doc = redfish.Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "NVIDIA", "Model": "BlueField-3"}
				case "/redfish/v1/Chassis":
					doc = redfish.Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Chassis/Card")}}
				case "/redfish/v1/Chassis/Card":
					cardReads++
					serial := "CARD-A"
					if scenario.replace && cardReads > 1 {
						serial = "CARD-B"
					}
					doc = redfish.Document{"Model": "BlueField-3", "SerialNumber": serial, "Links": redfish.Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}}
				case "/redfish/v1/UpdateService":
					doc = redfish.Document{"FirmwareInventory": link("/redfish/v1/UpdateService/FirmwareInventory")}
				case "/redfish/v1/UpdateService/FirmwareInventory":
					members := []any{link("/redfish/v1/UpdateService/FirmwareInventory/NIC")}
					if scenario.partial {
						members = append(members, link("/redfish/v1/UpdateService/FirmwareInventory/Denied"))
					}
					doc = redfish.Document{"Members@odata.count": len(members), "Members": members}
				case "/redfish/v1/UpdateService/FirmwareInventory/NIC":
					links := redfish.Document{scenario.activation: []any{link("/redfish/v1/Systems/DPU")}}
					if scenario.activation == "both" {
						links = redfish.Document{"ActiveTargets": []any{link("/redfish/v1/Systems/DPU")}, "StagedTargets": []any{link("/redfish/v1/Systems/DPU")}}
					}
					doc = redfish.Document{"Id": "NIC_Firmware", "Name": "NIC Firmware", "Version": "32.38.1002", "Links": links}
				case "/redfish/v1/UpdateService/FirmwareInventory/Denied":
					w.WriteHeader(http.StatusForbidden)
					return
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(doc)
			}))
			source := fmt.Sprintf("management:\n  kubeconfigRef: {file: unused}\ncredentials:\n  read: {username: reader, passwordRef: {env: DPF_SOFTWARE_TEST_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n", server.URL)
			in, e := config.Decode([]byte(source), filepath.Join(dir, "site.yaml"))
			Expect(e).NotTo(HaveOccurred())
			request, e := config.Resolve(in)
			Expect(e).NotTo(HaveOccurred())
			d, _, e := (redfish.Collector{}).Collect(context.Background(), request.Targets[0], time.Second)
			if scenario.replace {
				Expect(redfish.Code(e)).To(Equal("IDENTITY_CHANGED"))
			} else {
				Expect(e).NotTo(HaveOccurred())
			}
			server.Close()
			Expect(writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
			// Exact board product is independent SSH evidence; a generic Redfish family
			// is intentionally insufficient for production BOM applicability.
			d.Runtime = &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{"board-product": {State: "observed", Values: []string{"B3210"}}}}
			release, e := releases.Resolve("")
			Expect(e).NotTo(HaveOccurred())
			inv := &inventory.Inventory{Software: &release, DPUs: []inventory.Device{d}}
			component := "nic"
			if scenario.replace {
				component = "applicability"
			}
			check := softwareCheck(SoftwareChecks(inv), component)
			Expect(check.ReasonCode).To(Equal(scenario.expected), scenario.name)
			if scenario.name == "running" {
				Expect(check.State).To(Equal("passed"))
			} else {
				Expect(check.State).To(Equal("unknown"))
			}
		}
	})
})
