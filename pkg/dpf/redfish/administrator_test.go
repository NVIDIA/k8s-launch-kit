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

var _ = Describe("DPU BMC administrator evidence", func() {
	It("verifies the authenticated account and effective privileges without password or privilege mutations", func() {
		Expect(os.Setenv("DPF_ADMIN_TEST_PASSWORD", "credential-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_ADMIN_TEST_PASSWORD")
		for _, scenario := range []struct{ name, expected string }{
			{"administrator", "BMC_ADMIN_VERIFIED"}, {"custom full role", "BMC_ADMIN_VERIFIED"}, {"absolute links", "BMC_ADMIN_VERIFIED"}, {"unrelated account unreadable", "BMC_ADMIN_VERIFIED"}, {"session role absent", "BMC_ADMIN_VERIFIED"}, {"session password change", "BMC_PASSWORD_CHANGE_REQUIRED"}, {"malformed pagination", "BMC_ADMIN_UNVERIFIABLE"}, {"late password change", "BMC_PASSWORD_CHANGE_REQUIRED"},
			{"username admin read only", "BMC_ADMIN_REQUIRED"}, {"operator", "BMC_ADMIN_REQUIRED"}, {"OEM only", "BMC_ADMIN_REQUIRED"},
			{"account service denied", "BMC_ADMIN_ACCESS_DENIED"}, {"account member denied", "BMC_ADMIN_ACCESS_DENIED"}, {"role denied", "BMC_ADMIN_ACCESS_DENIED"},
			{"account service missing", "BMC_ADMIN_UNVERIFIABLE"}, {"missing user", "BMC_ADMIN_UNVERIFIABLE"}, {"duplicate user", "BMC_ADMIN_UNVERIFIABLE"},
			{"malformed privileges", "BMC_ADMIN_UNVERIFIABLE"}, {"duplicate privileges", "BMC_ADMIN_UNVERIFIABLE"}, {"missing state", "BMC_ADMIN_UNVERIFIABLE"},
			{"disabled", "BMC_ADMIN_ACCOUNT_RESTRICTED"}, {"locked", "BMC_ADMIN_ACCOUNT_RESTRICTED"}, {"password change", "BMC_PASSWORD_CHANGE_REQUIRED"},
			{"login password change", "BMC_PASSWORD_CHANGE_REQUIRED"}, {"denied password change", "BMC_PASSWORD_CHANGE_REQUIRED"},
			{"session identity mismatch", "BMC_ADMIN_UNVERIFIABLE"}, {"session role mismatch", "BMC_ADMIN_UNVERIFIABLE"}, {"role identity mismatch", "BMC_ADMIN_UNVERIFIABLE"},
			{"foreign service", "BMC_ADMIN_UNVERIFIABLE"}, {"foreign role", "BMC_ADMIN_UNVERIFIABLE"}, {"foreign account", "BMC_ADMIN_UNVERIFIABLE"},
			{"wrong account tree", "BMC_ADMIN_UNVERIFIABLE"}, {"query role", "BMC_ADMIN_UNVERIFIABLE"}, {"incomplete accounts", "BMC_ADMIN_UNVERIFIABLE"},
		} {
			By(scenario.name)
			writes, visited := []string{}, []string{}
			username, roleID := "site-maintainer", "Administrator"
			if scenario.name == "username admin read only" || scenario.name == "unrelated account unreadable" {
				username = "admin"
			}
			if scenario.name == "custom full role" {
				roleID = "SiteMaintainer"
			}
			link := func(uri string) Document { return Document{"@odata.id": uri} }
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				visited = append(visited, r.URL.Path)
				if r.Method != http.MethodGet {
					writes = append(writes, r.Method+" "+r.URL.Path)
				}
				var doc Document
				switch r.URL.Path {
				case "/redfish/v1/":
					doc = Document{"Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}, "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "AccountService": link("/redfish/v1/AccountService")}
					if scenario.name == "late password change" {
						doc["UpdateService"] = link("/redfish/v1/UpdateService")
					}
					if scenario.name == "account service missing" {
						delete(doc, "AccountService")
					}
					if scenario.name == "foreign service" {
						doc["AccountService"] = link("https://foreign.invalid/redfish/v1/AccountService")
					}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "token-canary")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
					w.WriteHeader(http.StatusCreated)
					doc = Document{}
					if scenario.name == "login password change" {
						doc["@Message.ExtendedInfo"] = []any{Document{"MessageId": "Base.1.18.PasswordChangeRequired", "Message": "credential-canary"}}
					}
				case "/redfish/v1/SessionService/Sessions/owned":
					if r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					doc = Document{"UserName": username, "Roles": []any{roleID}}
					if scenario.name == "session password change" {
						doc["@Message.ExtendedInfo"] = []any{Document{"MessageId": "Base.1.18.PasswordChangeRequired"}}
					}
					if scenario.name == "session role absent" {
						delete(doc, "Roles")
					}
					if scenario.name == "session identity mismatch" {
						doc["UserName"] = "someone-else"
					}
					if scenario.name == "session role mismatch" {
						doc["Roles"] = []any{"ReadOnly"}
					}
				case "/redfish/v1/AccountService":
					if scenario.name == "account service denied" || scenario.name == "denied password change" {
						w.WriteHeader(http.StatusForbidden)
						if scenario.name == "denied password change" {
							json.NewEncoder(w).Encode(Document{"error": Document{"@Message.ExtendedInfo": []any{Document{"MessageId": "Base.1.18.PasswordChangeRequired", "Message": "credential-canary"}}}})
						}
						return
					}
					doc = Document{"Accounts": link("/redfish/v1/AccountService/Accounts")}
				case "/redfish/v1/AccountService/Accounts":
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/AccountService/Accounts/7")}}
					if scenario.name == "unrelated account unreadable" {
						doc["Members"] = []any{link("/redfish/v1/AccountService/Accounts/root"), link("/redfish/v1/AccountService/Accounts/admin")}
						doc["Members@odata.count"] = 2
					}
					if scenario.name == "malformed pagination" {
						doc["Members@odata.nextLink"] = false
					}
					if scenario.name == "duplicate user" {
						doc["Members"] = []any{link("/redfish/v1/AccountService/Accounts/7"), link("/redfish/v1/AccountService/Accounts/8")}
						doc["Members@odata.count"] = 2
					}
					if scenario.name == "foreign account" {
						doc["Members"] = []any{link("https://foreign.invalid/redfish/v1/AccountService/Accounts/7")}
					}
					if scenario.name == "wrong account tree" {
						doc["Members"] = []any{link("/redfish/v1/Systems/unrelated")}
					}
					if scenario.name == "incomplete accounts" {
						doc["Members@odata.count"] = 2
					}
				case "/redfish/v1/AccountService/Accounts/root":
					w.WriteHeader(http.StatusInternalServerError)
					return
				case "/redfish/v1/AccountService/Accounts/7", "/redfish/v1/AccountService/Accounts/8", "/redfish/v1/AccountService/Accounts/admin":
					if scenario.name == "account member denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					doc = Document{"UserName": username, "Enabled": true, "Locked": false, "PasswordChangeRequired": false, "RoleId": roleID, "Links": Document{"Role": link("/redfish/v1/AccountService/Roles/" + roleID)}}
					switch scenario.name {
					case "missing user":
						doc["UserName"] = "other"
					case "missing state":
						delete(doc, "PasswordChangeRequired")
					case "disabled":
						doc["Enabled"] = false
					case "locked":
						doc["Locked"] = true
					case "password change":
						doc["PasswordChangeRequired"] = true
					case "foreign role":
						doc["Links"] = Document{"Role": link("https://foreign.invalid/redfish/v1/AccountService/Roles/" + roleID)}
					case "query role":
						doc["Links"] = Document{"Role": link("/redfish/v1/AccountService/Roles/" + roleID + "?$skip=1")}
					}
				case "/redfish/v1/AccountService/Roles/Administrator", "/redfish/v1/AccountService/Roles/SiteMaintainer":
					if scenario.name == "role denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					doc = Document{"Id": roleID, "IsPredefined": roleID == "Administrator", "AssignedPrivileges": []any{"Login", "ConfigureManager", "ConfigureUsers", "ConfigureComponents", "ConfigureSelf"}}
					switch scenario.name {
					case "username admin read only", "operator", "OEM only":
						doc["AssignedPrivileges"] = []any{"Login", "ConfigureSelf"}
						doc["OemPrivileges"] = []any{"Administrator"}
					case "malformed privileges":
						doc["AssignedPrivileges"] = "Administrator"
					case "duplicate privileges":
						doc["AssignedPrivileges"] = []any{"Login", "Login", "ConfigureManager", "ConfigureUsers", "ConfigureComponents", "ConfigureSelf"}
					case "role identity mismatch":
						doc["Id"] = "OtherRole"
					}
				case "/redfish/v1/UpdateService":
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(Document{"error": Document{"@Message.ExtendedInfo": []any{Document{"MessageId": "Base.1.18.PasswordChangeRequired", "Message": "credential-canary"}}}})
					return
				case "/redfish/v1/Systems":
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/DPU")}}
				case "/redfish/v1/Systems/DPU":
					doc = Document{"UUID": "11111111-1111-4111-8111-111111111111", "Manufacturer": "NVIDIA", "Model": "BlueField-3"}
				case "/redfish/v1/Chassis":
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Chassis/Card")}}
				case "/redfish/v1/Chassis/Card":
					doc = Document{"Model": "BlueField-3", "SerialNumber": "CARD-A", "Links": Document{"ComputerSystems": []any{link("/redfish/v1/Systems/DPU")}}}
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if scenario.name == "absolute links" {
					absoluteLinks(doc, "https://"+r.Host)
				}
				json.NewEncoder(w).Encode(doc)
			}))
			source := fmt.Sprintf("credentials:\n  read: {username: '%s', passwordRef: {env: DPF_ADMIN_TEST_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n  probes: {mode: inventory}\n", username, server.URL)
			input, err := config.Decode([]byte(source), filepath.Join(GinkgoT().TempDir(), "site.yaml"))
			Expect(err).NotTo(HaveOccurred())
			request, err := config.Resolve(input)
			Expect(err).NotTo(HaveOccurred())
			device, evidence, err := (Collector{}).Collect(context.Background(), request.Targets[0], time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(device.ObservationState).To(Equal("observed"))
			Expect(device.SerialNumber).To(Equal("CARD-A"))
			admin := device.Observations["bmc-administrator"]
			Expect(admin.ReasonCode).To(Equal(scenario.expected))
			Expect(admin.Message).NotTo(BeEmpty())
			if scenario.expected == "BMC_ADMIN_VERIFIED" {
				Expect(admin.State).To(Equal("observed"))
			} else {
				Expect(admin.State).To(Equal("failed"))
			}
			encoded, err := json.Marshal([]any{device, evidence})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(encoded)).NotTo(ContainSubstring("credential-canary"))
			Expect(string(encoded)).NotTo(ContainSubstring("token-canary"))
			Expect(string(encoded)).NotTo(ContainSubstring(`"` + username + `"`))
			Expect(visited).NotTo(ContainElement("/redfish/v1/Systems/unrelated"))
			Expect(visited).NotTo(ContainElement("/redfish/v1/AccountService/Accounts/root"))
			Expect(writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
			server.Close()
		}
	})
})
