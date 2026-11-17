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
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func networkFunctionInventoryServer(mutate func(string, int, Document), failDelete bool, denyFunctions bool) (*httptest.Server, func() []string) {
	link := func(uri string) Document { return Document{"@odata.id": uri} }
	collection := func(uris ...string) Document {
		members := make([]any, 0, len(uris))
		for _, uri := range uris {
			members = append(members, link(uri))
		}
		return Document{"Members@odata.count": len(uris), "Members": members}
	}
	const base = "/redfish/v1/Chassis/Board/NetworkAdapters/NIC0"
	docs := map[string]Document{
		"/redfish/v1/":                              {"Vendor": "NVIDIA", "Product": "BlueField-4", "Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}, "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis")},
		"/redfish/v1/Systems":                       collection("/redfish/v1/Systems/DPU"),
		"/redfish/v1/Systems/DPU":                   {"Manufacturer": "Nvidia", "Model": "BlueField-4", "SerialNumber": "CARD-A", "UUID": "11111111-1111-4111-8111-111111111111", "Links": Document{"Chassis": []any{link("/redfish/v1/Chassis/Board")}}},
		"/redfish/v1/Chassis":                       collection("/redfish/v1/Chassis/Board", "/redfish/v1/Chassis/BMC"),
		"/redfish/v1/Chassis/Board":                 {"ChassisType": "Card", "Manufacturer": "NVIDIA", "Model": "B4240", "SerialNumber": "CARD-A", "Links": Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}, "NetworkAdapters": link("/redfish/v1/Chassis/Board/NetworkAdapters")},
		"/redfish/v1/Chassis/BMC":                   {"ChassisType": "Component", "Manufacturer": "NVIDIA", "Model": "BlueField-4", "SerialNumber": "CARD-A", "Links": Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}},
		"/redfish/v1/Chassis/Board/NetworkAdapters": collection(base),
		base:                               {"Id": "NIC0", "NetworkDeviceFunctions": link(base + "/NetworkDeviceFunctions")},
		base + "/NetworkDeviceFunctions":   collection(base+"/NetworkDeviceFunctions/0", base+"/NetworkDeviceFunctions/1"),
		base + "/NetworkDeviceFunctions/0": {"Id": "0", "NetDevFuncType": "Ethernet", "Ethernet": Document{"PermanentMACAddress": "F4:20:4D:16:19:B0"}},
		base + "/NetworkDeviceFunctions/1": {"Id": "1", "NetDevFuncType": "Ethernet", "Ethernet": Document{"PermanentMACAddress": "F4:20:4D:16:19:B1"}},
	}
	reads := map[string]int{}
	methods := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path != "/redfish/v1/" {
			path = strings.TrimSuffix(path, "/")
		}
		methods = append(methods, r.Method+" "+path)
		if path == "/redfish/v1/SessionService/Sessions" && r.Method == http.MethodPost {
			w.Header().Set("X-Auth-Token", "fixture-token")
			w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
			w.WriteHeader(http.StatusCreated)
			return
		}
		if path == "/redfish/v1/SessionService/Sessions/owned" {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(Document{"UserName": "reader"})
			case http.MethodDelete:
				if failDelete {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		if denyFunctions && path == base+"/NetworkDeviceFunctions" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		doc, ok := docs[path]
		if !ok || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		reads[path]++
		if mutate != nil {
			mutate(path, reads[path], doc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	return server, func() []string { return slicesClone(methods) }
}

func slicesClone(values []string) []string {
	return append([]string(nil), values...)
}

func networkFunctionInventoryTarget(serverURL string) config.Target {
	seed := fmt.Sprintf("credentials:\n  read: {username: reader, passwordRef: {env: DPF_NETWORK_FUNCTION_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n  probes: {mode: inventory}\n", serverURL)
	input, err := config.Decode([]byte(seed), filepath.Join(GinkgoT().TempDir(), "network-functions.yaml"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	request, err := config.Resolve(input)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return request.Targets[0]
}

var _ = Describe("optional NetworkDeviceFunction MAC inventory", func() {
	It("collects only identity-selected DPU MACs and keeps the session read-only", func() {
		Expect(os.Setenv("DPF_NETWORK_FUNCTION_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_NETWORK_FUNCTION_PASSWORD")
		server, methods := networkFunctionInventoryServer(nil, false, false)
		defer server.Close()
		target := networkFunctionInventoryTarget(server.URL)

		device, _, err := (Collector{IncludeNetworkFunctionMACs: true}).Collect(context.Background(), target, 3*time.Second)

		Expect(err).NotTo(HaveOccurred())
		Expect(device.ID).NotTo(BeEmpty())
		Expect(device.SerialNumber).To(Equal("CARD-A"))
		Expect(device.NetworkFunctionMACs).To(Equal([]string{"f4:20:4d:16:19:b0", "f4:20:4d:16:19:b1"}))
		Expect(device.Observations["networkFunctionMACs"].State).To(Equal("observed"))
		for _, request := range methods() {
			if strings.HasPrefix(request, "POST /redfish/v1/SessionService/Sessions") || strings.HasPrefix(request, "DELETE /redfish/v1/SessionService/Sessions/") {
				continue
			}
			Expect(request).NotTo(ContainSubstring(" PATCH "))
			Expect(request).NotTo(ContainSubstring(" PUT "))
			Expect(request).NotTo(ContainSubstring(" DELETE "))
			if !strings.HasPrefix(request, "POST ") && !strings.HasPrefix(request, "DELETE ") {
				Expect(request).To(HavePrefix("GET "))
			}
		}
	})

	It("retains a specific read failure without failing unrelated DPU inventory", func() {
		Expect(os.Setenv("DPF_NETWORK_FUNCTION_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_NETWORK_FUNCTION_PASSWORD")
		server, _ := networkFunctionInventoryServer(nil, false, true)
		defer server.Close()
		device, _, err := (Collector{IncludeNetworkFunctionMACs: true}).Collect(context.Background(), networkFunctionInventoryTarget(server.URL), 3*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(device.ID).NotTo(BeEmpty())
		Expect(device.NetworkFunctionMACs).To(BeEmpty())
		Expect(device.Observations["networkFunctionMACs"].ReasonCode).To(Equal("ACCESS_DENIED"))
	})

	It("invalidates successful MACs if the outer card identity recheck or session cleanup fails", func() {
		Expect(os.Setenv("DPF_NETWORK_FUNCTION_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_NETWORK_FUNCTION_PASSWORD")
		for _, mode := range []string{"identity", "cleanup"} {
			By(mode)
			mutate := func(path string, read int, doc Document) {
				if mode == "identity" && path == "/redfish/v1/Chassis/Board" && read == 3 {
					doc["Model"] = "B4240-REPLACED"
				}
			}
			server, _ := networkFunctionInventoryServer(mutate, mode == "cleanup", false)
			device, _, err := (Collector{IncludeNetworkFunctionMACs: true}).Collect(context.Background(), networkFunctionInventoryTarget(server.URL), 3*time.Second)
			server.Close()
			Expect(Code(err)).NotTo(BeEmpty())
			Expect(device.NetworkFunctionMACs).To(BeEmpty())
			Expect(device.Observations["networkFunctionMACs"].ReasonCode).To(Equal("NETWORK_FUNCTION_MAC_RECHECK_FAILED"))
			wire, outputErr := inventory.DeviceToOutput(device, "dpus")
			Expect(outputErr).NotTo(HaveOccurred())
			Expect(wire.NetworkFunctionMACs).To(BeEmpty())
			found := false
			for _, issue := range wire.Issues {
				found = found || issue.Field == "networkFunctionMACs" && issue.Code == "NETWORK_FUNCTION_MAC_RECHECK_FAILED" && issue.Tool == "redfish"
			}
			Expect(found).To(BeTrue())
		}
	})
})
