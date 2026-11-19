// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

type validationCablingCapturer func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error)

func (f validationCablingCapturer) CaptureHostLLDP(ctx context.Context, target config.Target, request inventory.HostLLDPCaptureRequest, timeout time.Duration) (inventory.HostLLDPCapture, error) {
	return f(ctx, target, request, timeout)
}

// This server is reached by the observer's default Redfish reader, including
// ordinary TLS verification, owned session cleanup and identity rechecks.
func TestValidationDefaultPhysicalReaderCollectsMACsOnlyForCabling(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	o.physical.bmc = nil
	link := func(uri string) redfish.Document { return redfish.Document{"@odata.id": uri} }
	collection := func(uri string) redfish.Document {
		return redfish.Document{"Members@odata.count": 1, "Members": []any{link(uri)}}
	}
	const card = "/redfish/v1/Chassis/Card"
	const adapter = card + "/NetworkAdapters/NIC0"
	const function = adapter + "/NetworkDeviceFunctions/0"
	docs := map[string]redfish.Document{
		"/redfish/v1/":                      {"Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "Links": redfish.Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}},
		"/redfish/v1/Systems":               collection("/redfish/v1/Systems/DPU"),
		"/redfish/v1/Systems/DPU":           {"Manufacturer": "NVIDIA", "Model": "BlueField-3", "UUID": "11111111-1111-4111-8111-111111111111"},
		"/redfish/v1/Chassis":               collection(card),
		card:                                {"Model": "BlueField-3", "SerialNumber": "CARD-A", "Links": redfish.Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}, "NetworkAdapters": link(card + "/NetworkAdapters")},
		card + "/NetworkAdapters":           collection(adapter),
		adapter:                             {"Id": "NIC0", "NetworkDeviceFunctions": link(adapter + "/NetworkDeviceFunctions")},
		adapter + "/NetworkDeviceFunctions": collection(function),
		function:                            {"Id": "0", "NetDevFuncType": "Ethernet", "Ethernet": redfish.Document{"PermanentMACAddress": "F4:20:4D:16:19:B0"}},
	}
	var mu sync.Mutex
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.Path
		if uri != "/redfish/v1/" {
			uri = strings.TrimSuffix(uri, "/")
		}
		mu.Lock()
		requests = append(requests, r.Method+" "+uri)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if uri == "/redfish/v1/SessionService/Sessions" && r.Method == http.MethodPost {
			w.Header().Set("X-Auth-Token", "fixture-token")
			w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
			w.WriteHeader(http.StatusCreated)
			return
		}
		if uri == "/redfish/v1/SessionService/Sessions/owned" {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(redfish.Document{"UserName": "reader"})
				return
			}
		}
		if doc, ok := docs[uri]; ok && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DPF_VALIDATION_CABLING_PASSWORD", "fixture-password")
	source := fmt.Sprintf("credentials:\n  read: {username: reader, passwordRef: {env: DPF_VALIDATION_CABLING_PASSWORD}}\ndiscovery:\n  dpus: [{bmcEndpoint: %s, credentialRef: read, tls: {caFile: ca.pem}}]\n  hosts: [{bmcEndpoint: https://192.0.2.1, credentialRef: read}]\n", server.URL)
	input, err := config.Decode([]byte(source), filepath.Join(dir, "input.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := config.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, includeCabling := range []bool{true, false} {
		t.Run(fmt.Sprintf("cabling=%t", includeCabling), func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()
			device, _, err := o.physicalReaders(includeCabling).bmc(context.Background(), request.Targets[0], 3*time.Second)
			if err != nil || device.ObservationState != "observed" {
				t.Fatalf("default physical BMC reader: state=%s error=%v", device.ObservationState, err)
			}
			mu.Lock()
			observed := append([]string(nil), requests...)
			mu.Unlock()
			functionReads, sessionDeletes := 0, 0
			for _, method := range observed {
				if strings.Contains(method, "/NetworkDeviceFunctions") {
					functionReads++
				}
				if method == "DELETE /redfish/v1/SessionService/Sessions/owned" {
					sessionDeletes++
					continue
				}
				if method != "POST /redfish/v1/SessionService/Sessions" && !strings.HasPrefix(method, "GET ") {
					t.Fatalf("unexpected physical write: %s", method)
				}
			}
			if sessionDeletes != 1 {
				t.Fatalf("owned session cleanup count=%d", sessionDeletes)
			}
			if includeCabling {
				if functionReads == 0 || !reflect.DeepEqual(device.NetworkFunctionMACs, []string{"f4:20:4d:16:19:b0"}) || device.Observations["networkFunctionMACs"].State != "observed" {
					t.Fatalf("resource phase omitted MAC ownership: reads=%d macs=%v", functionReads, device.NetworkFunctionMACs)
				}
			} else if functionReads != 0 || len(device.NetworkFunctionMACs) != 0 {
				t.Fatalf("boot-fence reader performed cabling reads: reads=%d macs=%v", functionReads, device.NetworkFunctionMACs)
			}
		})
	}
	t.Run("runtime fence uses default reader without MAC graph", func(t *testing.T) {
		// The HTTP fixture deliberately differs from retained identity, so entry
		// must reject it. The read still exercises the actual runtime callsite.
		o.physicalScope.expected.DPUs[0].BMCEndpoint = server.URL
		targets := []config.Target{request.Targets[0]}
		for _, target := range o.request.Targets {
			if target.Role == "hosts" {
				targets = append(targets, target)
			}
		}
		o.request.Targets = targets
		mu.Lock()
		requests = nil
		mu.Unlock()
		if _, err := o.openValidationPhysicalFence(context.Background()); err == nil {
			t.Fatal("runtime fence accepted a different physical device")
		}
		mu.Lock()
		observed := append([]string(nil), requests...)
		mu.Unlock()
		closed := false
		for _, method := range observed {
			if strings.Contains(method, "/NetworkDeviceFunctions") {
				t.Fatalf("runtime entry read the cabling MAC graph: %s", method)
			}
			closed = closed || method == "DELETE /redfish/v1/SessionService/Sessions/owned"
		}
		if !closed {
			t.Fatal("runtime entry did not complete the real default BMC read and session cleanup")
		}
	})
}

func validationCablingFixture(t *testing.T) (*validationObserver, map[string]inventory.Device, map[string]inventory.Device) {
	t.Helper()
	o, bmcs, hosts := physicalValidationFixture(t)
	ports := []config.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}, {Name: "swp2", DPUSerial: "SERIAL-A", DPUInterface: "p1"}}
	o.request.RequestTimeout = 4 * time.Second
	o.request.Switches = []config.SwitchTarget{{Name: "leaf-a", Endpoint: "ssh://leaf.example:22", Access: &config.SSHAccess{}, Ports: ports}}
	o.request.Input.Site.Discovery.Switches = []config.Switch{{Name: "leaf-a", SSH: config.SSH{Address: "leaf.example"}, Ports: ports}}
	o.checks = o.physicalValidationChecks()
	return o, bmcs, hosts
}

func validationCablingCapture(request inventory.HostLLDPCaptureRequest) inventory.HostLLDPCapture {
	capture := inventory.HostLLDPCapture{Ports: make([]inventory.HostLLDPCaptureResult, 0, len(request.Ports))}
	for i, port := range request.Ports {
		capture.Ports = append(capture.Ports, inventory.HostLLDPCaptureResult{
			PCIAddress: port.PCIAddress, Interface: port.Interface, PhysicalPortName: fmt.Sprintf("p%d", i),
			State: "observed", ReasonCode: "HOST_LLDP_PEER_OBSERVED", Message: "peer received",
			Neighbors: []inventory.HostLLDPNeighbor{{SourceMAC: "02:aa:00:00:01:02", ChassisIDSubtype: 4, ChassisID: "02:aa:00:00:01:01", PortIDSubtype: 5, PortID: fmt.Sprintf("swp%d", i+1), SystemName: "leaf-a"}},
		})
	}
	return capture
}

func TestValidationCablingCapturesAfterFreshSwitchReadAndPreservesConflicts(t *testing.T) {
	for _, scenario := range []string{"fallback", "BMC MAC conflict", "switch port conflict", "wrong host source", "unrelated captured interface"} {
		t.Run(scenario, func(t *testing.T) {
			o, bmcs, hosts := validationCablingFixture(t)
			freshSwitch, captures := false, 0
			mac, label := "02:00:00:00:00:bb", "p0"
			if scenario == "switch port conflict" {
				mac, label = "02:00:00:00:00:aa", "p1"
			}
			if scenario == "BMC MAC conflict" || scenario == "switch port conflict" {
				for endpoint, dpu := range bmcs {
					if dpu.SerialNumber == "SERIAL-A" {
						dpu.NetworkFunctionMACs = []string{"02:00:00:00:00:aa"}
						bmcs[endpoint] = dpu
					}
				}
			}
			if scenario == "wrong host source" {
				host := hosts["ssh://host-a.example.test:22"]
				host.InputRef = "unselected-host-source"
				hosts["ssh://host-a.example.test:22"] = host
			}
			o.physical.fabric = func(_ context.Context, target config.SwitchTarget, timeout time.Duration) fabric.Snapshot {
				if target.Name != "leaf-a" || target.Endpoint != "ssh://leaf.example:22" || timeout != o.request.RequestTimeout {
					t.Errorf("unexpected selected switch: %s %s timeout=%s", target.Name, target.Endpoint, timeout)
				}
				freshSwitch = true
				snapshot := fabric.Snapshot{Operational: fabric.OperationalFacts{LocalLLDP: &fabric.LLDPLocalIdentity{ChassisID: "02:aa:00:00:01:01", SystemName: "leaf-a"}}}
				if scenario == "BMC MAC conflict" || scenario == "switch port conflict" {
					snapshot.Operational.LLDP = []fabric.LLDPPeerHint{{LocalInterface: "swp1", PortID: &mac, PortName: &label}}
				}
				return snapshot
			}
			o.physical.lldp = validationCablingCapturer(func(_ context.Context, target config.Target, request inventory.HostLLDPCaptureRequest, timeout time.Duration) (inventory.HostLLDPCapture, error) {
				captures++
				if !freshSwitch {
					t.Error("LLDP capture preceded the fresh switch snapshot")
				}
				if target.Endpoint != "ssh://host-a.example.test:22" || target.InputRef != "host-input-a" || request.AdapterKey != "adapter-a" || request.AdapterSerial != "SERIAL-A" || request.AdapterPCIAddress != "0000:21:00.0" || request.HostUUID != hosts[target.Endpoint].SystemUUID || timeout != 4*time.Second {
					t.Errorf("capture escaped exact selected host/card or bounded timeout: target=%s/%s request=%+v timeout=%s", target.Endpoint, target.InputRef, request, timeout)
				}
				want := []inventory.HostLLDPCapturePort{{PCIAddress: "0000:21:00.0", Interface: "ens5f0np0"}, {PCIAddress: "0000:21:00.1", Interface: "ens5f1np1"}}
				if !reflect.DeepEqual(request.Ports, want) {
					t.Errorf("capture ports=%+v want %+v", request.Ports, want)
				}
				capture := validationCablingCapture(request)
				if scenario == "unrelated captured interface" {
					capture.Ports[0].Interface = "mgmt0"
				}
				return capture, nil
			})
			r := newValidationRecorder(o.checks, nil)
			if err := o.observePhysical(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			wantCaptures := 1
			if scenario == "wrong host source" {
				wantCaptures = 0
			}
			if !freshSwitch || captures != wantCaptures {
				t.Fatalf("freshSwitch=%t captures=%d want %d", freshSwitch, captures, wantCaptures)
			}
			for _, port := range []string{"swp1", "swp2"} {
				want := validation.Passed
				if scenario == "wrong host source" || scenario == "unrelated captured interface" {
					want = validation.Unverified
				} else if port == "swp1" && (scenario == "BMC MAC conflict" || scenario == "switch port conflict") {
					want = validation.Failed
				}
				result := r.outcomes[physicalID("fabric", "dpf.hbn.cabling/leaf-a/"+port)]
				if result.State != want {
					t.Errorf("%s cabling=%+v want %s", port, result, want)
				}
			}
		})
	}
}

func TestValidationBootFenceDoesNotCaptureLLDPOrReadSwitches(t *testing.T) {
	o, _, _ := validationCablingFixture(t)
	o.physical.lldp = validationCablingCapturer(func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error) {
		t.Error("runtime boot fence invoked LLDP packet capture")
		return inventory.HostLLDPCapture{}, nil
	})
	o.physical.fabric = func(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot {
		t.Error("runtime boot fence read switch cabling")
		return fabric.Snapshot{}
	}
	fence, err := o.openValidationPhysicalFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := fence(context.Background()); err != nil {
		t.Fatal(err)
	}
}
