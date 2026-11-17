// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type authBindingCollector struct{}

func (authBindingCollector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	if target.Role == "dpus" {
		return (redfish.Collector{}).Collect(ctx, target, timeout)
	}
	return (fixtureCollector{}).Collect(ctx, target, timeout)
}

var _ = Describe("administrator session binding through publication", func() {
	It("preserves a complete handoff when external or unknown authentication cannot establish privileges", func() {
		var scenario atomic.Int64
		link := func(uri string) redfish.Document { return redfish.Document{"@odata.id": uri} }
		collection := func(uri string) redfish.Document {
			return redfish.Document{"Members@odata.count": 1, "Members": []any{link(uri)}}
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const sessions = "/redfish/v1/SessionService/Sessions"
			if r.Method == http.MethodPost && r.URL.Path == sessions {
				w.Header().Set("X-Auth-Token", "fixture-session")
				w.Header().Set("Location", sessions+"/owned")
				w.WriteHeader(http.StatusCreated)
				// Creation metadata is not the effective role proof; use the GET.
				doc := redfish.Document{"Roles": []any{""}}
				if scenario.Load() == 4 {
					doc = redfish.Document{"Roles": []any{"ReadOnly"}, "UserAuthenticationSource": "ldaps://directory.example.invalid"}
				}
				_ = json.NewEncoder(w).Encode(doc)
				return
			}
			if r.Method == http.MethodDelete && r.URL.Path == sessions+"/owned" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var doc redfish.Document
			switch r.URL.Path {
			case "/redfish/v1/":
				doc = redfish.Document{"Links": redfish.Document{"Sessions": link(sessions)}, "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "AccountService": link("/redfish/v1/AccountService")}
			case sessions + "/owned":
				doc = redfish.Document{"UserName": "reader"}
				if scenario.Load() == 0 || scenario.Load() == 3 {
					doc["Roles"] = []any{"Administrator"}
				}
			case "/redfish/v1/AccountService":
				doc = redfish.Document{"Accounts": link("/redfish/v1/AccountService/Accounts")}
				if scenario.Load() == 1 {
					doc["LocalAccountAuth"] = "Disabled"
					doc["LDAP"] = redfish.Document{"ServiceEnabled": true, "RemoteRoleMapping": []any{redfish.Document{"RemoteUser": "reader", "LocalRole": "ReadOnly"}}}
				} else if scenario.Load() == 3 {
					doc["LocalAccountAuth"] = "Fallback"
				} else if scenario.Load() == 4 {
					// A changed service policy does not change the session's origin.
					doc["LocalAccountAuth"] = "Enabled"
				}
			case "/redfish/v1/AccountService/Accounts":
				doc = collection("/redfish/v1/AccountService/Accounts/reader")
			case "/redfish/v1/AccountService/Accounts/reader":
				doc = redfish.Document{"UserName": "reader", "Enabled": true, "Locked": false, "PasswordChangeRequired": false, "RoleId": "Administrator", "Links": redfish.Document{"Role": link("/redfish/v1/AccountService/Roles/Administrator")}}
			case "/redfish/v1/AccountService/Roles/Administrator":
				doc = redfish.Document{"Id": "Administrator", "AssignedPrivileges": []any{"Login", "ConfigureManager", "ConfigureUsers", "ConfigureComponents", "ConfigureSelf"}}
			case "/redfish/v1/Systems":
				doc = collection("/redfish/v1/Systems/DPU")
			case "/redfish/v1/Systems/DPU":
				doc = redfish.Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "NVIDIA", "Model": "BlueField-3"}
			case "/redfish/v1/Chassis":
				doc = collection("/redfish/v1/Chassis/Card")
			case "/redfish/v1/Chassis/Card":
				doc = redfish.Document{"Model": "BlueField-3", "SerialNumber": "CARD-ONE", "Links": redfish.Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}}
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		}))
		DeferCleanup(server.Close)
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		caPath := filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)).To(Succeed())
		Expect(os.Setenv("DPF_AUTH_BINDING_PASSWORD", "fixture-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_AUTH_BINDING_PASSWORD")
		seed := fmt.Sprintf("credentials:\n  reader: {username: reader, passwordRef: {env: DPF_AUTH_BINDING_PASSWORD}}\ndiscovery:\n  tls: {caFile: '%s'}\n  hosts: [{bmcEndpoint: 'https://host.invalid', credentialRef: reader}]\n  dpus: [{bmcEndpoint: '%s', credentialRef: reader}]\n  probes: {mode: inventory}\n", caPath, server.URL)
		inputPath := filepath.Join(dir, "seed.yaml")
		Expect(os.WriteFile(inputPath, []byte(seed), 0600)).To(Succeed())
		in, err := config.Load(inputPath)
		Expect(err).NotTo(HaveOccurred())
		req, err := config.Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		output := filepath.Join(dir, "discovered.yaml")
		var previous []byte
		for _, mode := range []int64{0, 1, 2, 3, 4} {
			scenario.Store(mode)
			store, err := artifacts.New(in, output, true)
			Expect(err).NotTo(HaveOccurred())
			report := Collect(context.Background(), req, store.RunID(), Dependencies{BMC: authBindingCollector{}})
			Expect(report.Errors).To(BeEmpty())
			Expect(report.Inventory.DPUs[0].ObservationState).To(Equal("observed"))
			Expect(report.Inventory.DPUs[0].SerialNumber).To(Equal("CARD-ONE"))
			Expect(report.Inventory.Associations).To(HaveLen(1))
			Expect(store.Publish(context.Background(), &report)).To(Succeed())
			current, err := os.ReadFile(output)
			Expect(err).NotTo(HaveOccurred())
			if mode == 0 {
				Expect(ExitCode(report)).To(Equal(0))
				Expect(report.Discovery.Outcome).To(Equal("complete"))
				previous = current
			} else {
				Expect(ExitCode(report)).To(Equal(2))
				Expect(report.Discovery.Outcome).To(Equal("partial"))
				Expect(report.Inventory.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_UNVERIFIABLE"))
				Expect(report.Discovery.Checks).To(ContainElement(And(HaveField("ID", "dpf.management.bmc-administrator"), HaveField("State", "failed"), HaveField("ReasonCode", "BMC_ADMIN_UNVERIFIABLE"), HaveField("Message", Not(BeEmpty())))))
				Expect(current).To(Equal(previous))
				Expect(report.Artifacts).To(ContainElement(HaveField("Path", filepath.Join(store.RunDir, "partial.yaml"))))
				persisted, err := os.ReadFile(filepath.Join(store.RunDir, "report.json"))
				Expect(err).NotTo(HaveOccurred())
				Expect(string(persisted)).To(ContainSubstring("BMC_ADMIN_UNVERIFIABLE"))
				Expect(string(persisted)).NotTo(ContainSubstring("fixture-password"))
				Expect(string(persisted)).NotTo(ContainSubstring("fixture-session"))
			}
		}
	})
})
