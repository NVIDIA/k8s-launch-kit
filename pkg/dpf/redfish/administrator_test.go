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
		for _, scenario := range []struct {
			name, expected          string
			session, service, login Document
			omitRoles, customRole   bool
		}{
			{name: "administrator", expected: "BMC_ADMIN_VERIFIED"}, {name: "custom full role", expected: "BMC_ADMIN_VERIFIED"}, {name: "absolute links", expected: "BMC_ADMIN_VERIFIED"}, {name: "unrelated account unreadable", expected: "BMC_ADMIN_VERIFIED"}, {name: "slash-terminated account member", expected: "BMC_ADMIN_VERIFIED"}, {name: "session role absent", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "session password change", expected: "BMC_PASSWORD_CHANGE_REQUIRED"}, {name: "malformed pagination", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "late password change", expected: "BMC_PASSWORD_CHANGE_REQUIRED"},
			{name: "username admin read only", expected: "BMC_ADMIN_REQUIRED"}, {name: "operator", expected: "BMC_ADMIN_REQUIRED"}, {name: "OEM only", expected: "BMC_ADMIN_REQUIRED"},
			{name: "account service denied", expected: "BMC_ADMIN_ACCESS_DENIED"}, {name: "account member denied", expected: "BMC_ADMIN_ACCESS_DENIED"}, {name: "role denied", expected: "BMC_ADMIN_ACCESS_DENIED"},
			{name: "account service missing", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "missing user", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "duplicate user", expected: "BMC_ADMIN_UNVERIFIABLE"},
			{name: "malformed privileges", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "duplicate privileges", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "missing state", expected: "BMC_ADMIN_UNVERIFIABLE"},
			{name: "disabled", expected: "BMC_ADMIN_ACCOUNT_RESTRICTED"}, {name: "locked", expected: "BMC_ADMIN_ACCOUNT_RESTRICTED"}, {name: "password change", expected: "BMC_PASSWORD_CHANGE_REQUIRED"},
			{name: "login password change", expected: "BMC_PASSWORD_CHANGE_REQUIRED"}, {name: "denied password change", expected: "BMC_PASSWORD_CHANGE_REQUIRED"},
			{name: "session identity mismatch", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "session role mismatch", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "role identity mismatch", expected: "BMC_ADMIN_UNVERIFIABLE"},
			{name: "foreign service", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "foreign role", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "foreign account", expected: "BMC_ADMIN_UNVERIFIABLE"},
			{name: "wrong account tree", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "query role", expected: "BMC_ADMIN_UNVERIFIABLE"}, {name: "incomplete accounts", expected: "BMC_ADMIN_UNVERIFIABLE"},
			{name: "local enabled roleless", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "Enabled"}},
			{name: "local enabled with LDAP roleless", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "Enabled", "LDAP": Document{"ServiceEnabled": true}}},
			{name: "enabled snapshot after external login", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "Enabled"}, login: Document{"Roles": []any{"ReadOnly"}, "UserAuthenticationSource": "ldaps://directory.example.invalid"}},
			{name: "enabled with effective session role", expected: "BMC_ADMIN_VERIFIED", service: Document{"LocalAccountAuth": "Enabled"}},
			{name: "enabled with explicit local source", expected: "BMC_ADMIN_VERIFIED", omitRoles: true, service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService"}},
			{name: "explicit local source roleless", expected: "BMC_ADMIN_VERIFIED", omitRoles: true, session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService"}},
			{name: "fallback explicit local source", expected: "BMC_ADMIN_VERIFIED", omitRoles: true, service: Document{"LocalAccountAuth": "Fallback"}, session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService"}},
			{name: "local first custom explicit source", expected: "BMC_ADMIN_VERIFIED", omitRoles: true, customRole: true, service: Document{"LocalAccountAuth": "LocalFirst"}, session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService"}},
			{name: "legacy LDAP certificate metadata", expected: "BMC_ADMIN_VERIFIED", service: Document{"LDAP": Document{"Certificates": Document{"@odata.id": "/redfish/v1/AccountService/LDAP/Certificates"}}}},
			{name: "unknown nullable source with session role", expected: "BMC_ADMIN_VERIFIED", session: Document{"UserAuthenticationSource": nil}},
			{name: "external local collision", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "Disabled", "LDAP": Document{"ServiceEnabled": true, "RemoteRoleMapping": []any{Document{"RemoteUser": "site-maintainer", "LocalRole": "ReadOnly"}}}}},
			{name: "disabled local with session role", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Disabled"}},
			{name: "disabled contradictory local source", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Disabled"}, session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService"}},
			{name: "fallback roleless", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "Fallback"}},
			{name: "fallback with session role only", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Fallback"}},
			{name: "local first roleless", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LocalAccountAuth": "LocalFirst"}},
			{name: "local first with session role only", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "LocalFirst"}},
			{name: "LDAP enabled missing session proof", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LDAP": Document{"ServiceEnabled": true}}},
			{name: "LDAP unknown missing session proof", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"LDAP": Document{"Certificates": Document{}}}},
			{name: "additional providers missing session proof", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, service: Document{"AdditionalExternalAccountProviders": Document{"@odata.id": "/redfish/v1/AccountService/ExternalAccountProviders"}}},
			{name: "external authentication source", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": "ldaps://directory.example.invalid"}},
			{name: "enabled contradictory external source", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"UserAuthenticationSource": "ldaps://directory.example.invalid"}},
			{name: "wrong local authentication resource", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService/Accounts/7"}},
			{name: "query authentication source", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": "/redfish/v1/AccountService?x=1"}},
			{name: "empty authentication source", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": ""}},
			{name: "boolean authentication source", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": true}},
			{name: "object authentication source", expected: "BMC_ADMIN_UNVERIFIABLE", session: Document{"UserAuthenticationSource": Document{"@odata.id": "/redfish/v1/AccountService"}}},
			{name: "null source roleless", expected: "BMC_ADMIN_UNVERIFIABLE", omitRoles: true, session: Document{"UserAuthenticationSource": nil}},
			{name: "unknown local mode", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Other"}},
			{name: "empty local mode", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": ""}},
			{name: "null local mode", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": nil}},
			{name: "boolean local mode", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": false}},
			{name: "array local mode", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": []any{"Enabled"}}},
			{name: "null session roles", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": nil}},
			{name: "empty session roles", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": []any{}}},
			{name: "null session role member", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": []any{nil}}},
			{name: "empty session role member", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": []any{""}}},
			{name: "duplicate session role members", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": []any{"Administrator", "Administrator"}}},
			{name: "malformed session roles", expected: "BMC_ADMIN_UNVERIFIABLE", service: Document{"LocalAccountAuth": "Enabled"}, session: Document{"Roles": "Administrator"}},
		} {
			By(scenario.name)
			writes, visited := []string{}, []string{}
			username, roleID := "site-maintainer", "Administrator"
			if scenario.name == "username admin read only" || scenario.name == "unrelated account unreadable" {
				username = "admin"
			}
			if scenario.name == "custom full role" || scenario.customRole {
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
					for key, value := range scenario.login {
						doc[key] = value
					}
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
					if scenario.name == "session role absent" || scenario.omitRoles {
						delete(doc, "Roles")
					}
					if scenario.name == "session identity mismatch" {
						doc["UserName"] = "someone-else"
					}
					if scenario.name == "session role mismatch" {
						doc["Roles"] = []any{"ReadOnly"}
					}
					for key, value := range scenario.session {
						doc[key] = value
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
					for key, value := range scenario.service {
						doc[key] = value
					}
				case "/redfish/v1/AccountService/Accounts":
					if scenario.name == "slash-terminated account member" {
						doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/AccountService/Accounts/site-maintainer/")}}
					} else {
						doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/AccountService/Accounts/7")}}
					}
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
				case "/redfish/v1/AccountService/Accounts/7", "/redfish/v1/AccountService/Accounts/8", "/redfish/v1/AccountService/Accounts/admin", "/redfish/v1/AccountService/Accounts/site-maintainer/":
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
			expectNoRedfishEvidence(device, evidence)
			Expect(err).NotTo(HaveOccurred())
			Expect(device.ObservationState).To(Equal("observed"))
			Expect(device.SerialNumber).To(Equal("CARD-A"))
			admin := device.Observations["bmc-administrator"]
			Expect(admin.ReasonCode).To(Equal(scenario.expected))
			Expect(admin.Message).NotTo(BeEmpty())
			if scenario.expected == "BMC_ADMIN_VERIFIED" {
				Expect(admin.State).To(Equal("observed"))
				Expect(visited).To(ContainElement("/redfish/v1/AccountService/Roles/" + roleID))
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

var _ = Describe("slash-terminated account links", func() {
	It("canonicalizes the collection and direct or paginated member links before checking usernames", func() {
		const collection = "/redfish/v1/AccountService/Accounts"
		c := &Client{origin: "https://bmc.example.invalid"}
		canonical, err := c.accountLink(collection+"/", collection)
		Expect(err).NotTo(HaveOccurred())
		Expect(canonical).To(Equal(collection))

		for _, scenario := range []struct {
			name  string
			paged bool
		}{
			{name: "direct username-addressed member"},
			{name: "member on later page", paged: true},
		} {
			By(scenario.name)
			var visited []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				visited = append(visited, r.URL.Path)
				var doc Document
				switch r.URL.Path {
				case collection:
					if scenario.paged && r.URL.Query().Get("$skip") == "1" {
						doc = Document{"Members@odata.count": 2, "Members": []any{Document{"@odata.id": collection + "/7/"}}}
					} else if scenario.paged {
						doc = Document{"Members@odata.count": 2, "Members": []any{Document{"@odata.id": collection + "/8"}}, "Members@odata.nextLink": collection + "?$skip=1"}
					} else {
						doc = Document{"Members@odata.count": 1, "Members": []any{Document{"@odata.id": collection + "/site-maintainer/"}}}
					}
				case collection + "/8":
					doc = Document{"UserName": "someone-else"}
				case collection + "/7/", collection + "/site-maintainer/":
					doc = Document{"UserName": "site-maintainer"}
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(doc)
			}))
			client := &Client{origin: server.URL, http: server.Client()}
			accounts, err := client.accounts(context.Background(), collection, "site-maintainer")
			Expect(err).NotTo(HaveOccurred())
			matches := 0
			for _, account := range accounts {
				if Text(account.Document, "UserName") == "site-maintainer" {
					matches++
				}
			}
			Expect(matches).To(Equal(1), "URI spelling must not replace authenticated username verification")
			if scenario.paged {
				Expect(visited).To(ContainElement(collection + "/7/"))
			} else {
				Expect(visited).To(ContainElement(collection + "/site-maintainer/"))
			}
			server.Close()
		}
	})

	It("rejects duplicate member aliases and unsafe paths in direct and paginated lists", func() {
		const collection = "/redfish/v1/AccountService/Accounts"
		for _, scenario := range []struct {
			name    string
			members []any
			next    bool
			want    string
		}{
			{name: "duplicate terminal slash alias", members: []any{Document{"@odata.id": collection + "/7"}, Document{"@odata.id": collection + "/7/"}}, want: "DUPLICATE_MEMBER"},
			{name: "nested member path", members: []any{Document{"@odata.id": collection + "/7/child"}}, want: "UNSAFE_ACCOUNT_LINK"},
			{name: "query on member path", members: []any{Document{"@odata.id": collection + "/7/?$skip=1"}}, want: "UNSAFE_ACCOUNT_LINK"},
			{name: "double terminal slash", members: []any{Document{"@odata.id": collection + "/7//"}}, want: "UNSAFE_LINK"},
			{name: "duplicate aliases in paginated list", members: []any{Document{"@odata.id": collection + "/7"}, Document{"@odata.id": collection + "/7/"}}, next: true, want: "DUPLICATE_MEMBER"},
		} {
			By(scenario.name)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				doc := Document{"Members@odata.count": len(scenario.members), "Members": scenario.members}
				if scenario.next && r.URL.Query().Get("$skip") == "" {
					doc["Members@odata.count"] = len(scenario.members)
					doc["Members@odata.nextLink"] = collection + "?$skip=1"
				} else if scenario.next {
					delete(doc, "Members@odata.nextLink")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(doc)
			}))
			client := &Client{origin: server.URL, http: server.Client()}
			_, err := client.accounts(context.Background(), collection, "site-maintainer")
			Expect(Code(err)).To(Equal(scenario.want))
			server.Close()
		}
	})
})
