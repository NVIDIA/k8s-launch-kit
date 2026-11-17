// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("host-only Redfish refresh ownership", func() {
	It("rejects replacement at the same resource before publishing over the previous handoff", func() {
		serial := "CARD-A"
		link := func(uri string) redfish.Document { return redfish.Document{"@odata.id": uri} }
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var doc redfish.Document
			switch r.URL.Path {
			case "/redfish/v1/":
				doc = redfish.Document{"SessionService": link("/redfish/v1/SessionService"), "Systems": link("/redfish/v1/Systems")}
			case "/redfish/v1/SessionService":
				doc = redfish.Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}
			case "/redfish/v1/SessionService/Sessions":
				w.Header().Set("X-Auth-Token", "fixture")
				w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
				w.WriteHeader(http.StatusCreated)
				doc = redfish.Document{}
			case "/redfish/v1/SessionService/Sessions/owned":
				w.WriteHeader(http.StatusNoContent)
				return
			case "/redfish/v1/Systems":
				doc = redfish.Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/host")}}
			case "/redfish/v1/Systems/host":
				doc = redfish.Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "Dell", "Model": "PowerEdge", "NetworkInterfaces": link("/redfish/v1/Systems/host/NetworkInterfaces")}
			case "/redfish/v1/Systems/host/NetworkInterfaces":
				doc = redfish.Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/host/NetworkInterfaces/1")}}
			case "/redfish/v1/Systems/host/NetworkInterfaces/1":
				doc = redfish.Document{"Links": redfish.Document{"NetworkAdapter": link("/redfish/v1/Chassis/host/NetworkAdapters/slot1")}}
			case "/redfish/v1/Chassis/host/NetworkAdapters/slot1":
				doc = redfish.Document{"SerialNumber": serial, "Model": "BlueField-3"}
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		}))
		DeferCleanup(server.Close)
		dir, err := os.MkdirTemp("", "dpf-refresh-")
		Expect(err).NotTo(HaveOccurred())
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		Expect(os.Setenv("DPF_REFRESH_TEST_PASSWORD", "fixture")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_REFRESH_TEST_PASSWORD")
		seed := fmt.Sprintf("credentials:\n  reader: {username: reader, passwordRef: {env: DPF_REFRESH_TEST_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{bmcEndpoint: '%s', credentialRef: reader}]\n  probes: {mode: inventory}\n", server.URL)
		in, err := config.Decode([]byte(seed), filepath.Join(dir, "seed.yaml"))
		Expect(err).NotTo(HaveOccurred())
		req, err := config.Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		observe := func() inventory.Inventory {
			host, _, err := (redfish.Collector{}).Collect(context.Background(), req.Targets[0], time.Second)
			Expect(err).NotTo(HaveOccurred())
			inv := inventory.Inventory{Collection: inventory.Collection{Mode: "inventory"}, Hosts: []inventory.Device{host}, RequestedScope: []inventory.ScopeEndpoint{{Role: "hosts", Endpoint: server.URL, TLSInsecureSkipVerify: true}}}
			inv.DPUs = discovery.HostCards(inv.Hosts)
			inv.Associations, _ = discovery.Correlate(inv.DPUs, inv.Hosts)
			Expect(inv.DPUs).To(HaveLen(1))
			Expect(inv.Associations).To(HaveLen(1))
			discovery.Canonicalize(&inv)
			return inv
		}
		old := observe()
		for _, replacement := range []bool{false, true} {
			if replacement {
				serial = "CARD-B"
			}
			current := observe()
			Expect(current.ScopeDigest).To(Equal(old.ScopeDigest))
			Expect(current.DPUs[0].RecordKey).To(Equal(old.DPUs[0].RecordKey))
			Expect(inventory.IdentityConflict(&old, &current)).To(Equal(replacement))
			output := filepath.Join(dir, fmt.Sprintf("discovered-%t.yaml", replacement))
			data, err := config.Saved(in, &old, output)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(output, data, 0600)).To(Succeed())
			prior, err := config.Load(output)
			Expect(err).NotTo(HaveOccurred())
			store, err := artifacts.New(prior, output, true)
			Expect(err).NotTo(HaveOccurred())
			report := discovery.NewReport()
			report.Inventory = &current
			report.Discovery.Outcome = "complete"
			err = store.Publish(context.Background(), &report)
			if replacement {
				Expect(err).To(MatchError(ContainSubstring("physical identity")))
				unchanged, readErr := os.ReadFile(output)
				Expect(readErr).NotTo(HaveOccurred())
				Expect(unchanged).To(Equal(data))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	})
})
