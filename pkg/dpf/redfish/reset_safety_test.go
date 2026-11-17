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
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const resetSystem = "/redfish/v1/Systems/BlueField_0"

type resetFixture struct {
	docs        map[string]Document
	reads       map[string]int
	writes      []string
	cleanupFail bool
	mutate      func(string, int, Document)
}

func newResetFixture() *resetFixture {
	link := func(uri string) Document { return Document{"@odata.id": uri} }
	list := func(uris ...string) Document {
		members := []any{}
		for _, uri := range uris {
			members = append(members, link(uri))
		}
		return Document{"Members": members, "Members@odata.count": len(members)}
	}
	const inventory = "/redfish/v1/UpdateService/FirmwareInventory"
	f := &resetFixture{reads: map[string]int{}, writes: []string{}, docs: map[string]Document{
		"/redfish/v1": {"Vendor": "NVIDIA", "Product": "BlueField-4", "Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")},
			"Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "AccountService": link("/redfish/v1/AccountService"),
			"UpdateService": link("/redfish/v1/UpdateService"), "Tasks": link("/redfish/v1/TaskService")},
		"/redfish/v1/SessionService/Sessions/owned":      {"UserName": "admin", "Roles": []any{"Administrator"}},
		"/redfish/v1/AccountService":                     {"LocalAccountAuth": "Enabled", "Accounts": link("/redfish/v1/AccountService/Accounts")},
		"/redfish/v1/AccountService/Accounts":            list("/redfish/v1/AccountService/Accounts/admin"),
		"/redfish/v1/AccountService/Accounts/admin":      {"UserName": "admin", "Enabled": true, "Locked": false, "PasswordChangeRequired": false, "RoleId": "Administrator", "Links": Document{"Role": link("/redfish/v1/AccountService/Roles/Administrator")}},
		"/redfish/v1/AccountService/Roles/Administrator": {"Id": "Administrator", "AssignedPrivileges": []any{"Login", "ConfigureManager", "ConfigureUsers", "ConfigureComponents", "ConfigureSelf"}},
		"/redfish/v1/Systems":                            list(resetSystem),
		resetSystem: {"@odata.id": resetSystem, "Manufacturer": "NVIDIA", "Model": "BlueField-4", "SerialNumber": "SERIAL-A", "AssetTag": "MT_0000001774", "UUID": "11111111-1111-4111-8111-111111111111",
			"PowerState": "Off", "Status": Document{"State": "Disabled", "Health": "OK"}, "BootProgress": Document{"LastState": "None", "LastStateTime": "2026-11-13T13:00:00Z"}},
		"/redfish/v1/Chassis":           list("/redfish/v1/Chassis/card"),
		"/redfish/v1/Chassis/card":      {"PowerState": "On", "Status": Document{"State": "Enabled", "Health": "OK", "HealthRollup": "OK"}, "Manufacturer": "NVIDIA", "Model": "B4240", "ChassisType": "Card", "SerialNumber": "SERIAL-A", "Links": Document{"ComputerSystems": []any{link(resetSystem)}}},
		"/redfish/v1/TaskService":       {"Tasks": link("/redfish/v1/TaskService/Tasks")},
		"/redfish/v1/TaskService/Tasks": list("/redfish/v1/TaskService/Tasks/7"),
		"/redfish/v1/TaskService/Tasks/7": {"@odata.id": "/redfish/v1/TaskService/Tasks/7", "TaskState": "Completed", "TaskStatus": "OK", "PercentComplete": 100,
			"Messages": []any{Document{"Severity": "OK"}}, "Payload": Document{"private": "response-body-canary"}},
		"/redfish/v1/UpdateService": {"Status": Document{"State": "Enabled"}, "FirmwareInventory": link(inventory)},
	}}
	members := []string{}
	for _, id := range []string{"BMC", "CPU", "ERoT_BMC", "ERoT_CPU", "NIC"} {
		name := "BlueField_FW_" + id + "_0"
		version := "02.00.0048.0000_n05"
		if id == "CPU" {
			version = "26.10-0001"
		}
		if id == "NIC" {
			version = "82.48.4044"
		}
		if id == "BMC" {
			version = "BF4-26.10-0001"
		}
		active := Document{"FirmwareState": "Activated", "Version": version, "BuildType": "Release"}
		inactive := Document{"FirmwareState": "Inactive", "Version": version, "BuildType": "Release"}
		if id == "NIC" {
			active = Document{"FirmwareState": "Unknown", "Version": "NA", "BuildType": "Development"}
			inactive = Document{"FirmwareState": "Unknown", "Version": "NA", "BuildType": "Development"}
		}
		uri := inventory + "/" + name
		f.docs[uri] = Document{"@odata.id": uri, "Id": name, "Version": version, "Status": Document{"State": "Enabled"}, "Oem": Document{"Nvidia": Document{"ActiveFirmwareSlot": active, "InactiveFirmwareSlot": inactive}}}
		if id == "CPU" {
			f.docs[uri]["Status"] = Document{"State": "StandbyOffline", "Health": "Critical", "HealthRollup": "OK", "Conditions": []any{}}
		}
		pending := uri + "_pending"
		f.docs[pending] = Document{"@odata.id": pending, "Id": name + "_pending", "Version": ""}
		members = append(members, uri, pending)
	}
	f.docs[inventory] = list(members...)
	return f
}

func (f *resetFixture) handler(w http.ResponseWriter, r *http.Request) {
	uri := strings.TrimSuffix(r.URL.Path, "/")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+uri)
		switch {
		case r.Method == http.MethodPost && uri == "/redfish/v1/SessionService/Sessions":
			w.Header().Set("X-Auth-Token", "token-canary")
			w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && uri == "/redfish/v1/SessionService/Sessions/owned":
			if f.cleanupFail {
				w.WriteHeader(http.StatusForbidden)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	doc := f.docs[uri]
	if doc == nil {
		http.NotFound(w, r)
		return
	}
	f.reads[uri]++
	if f.mutate != nil {
		f.mutate(uri, f.reads[uri], doc)
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func resetFixtureTarget(endpoint string) config.Target {
	Expect(os.Setenv("RESET_BMC_PASSWORD", "credential-canary")).To(Succeed())
	DeferCleanup(os.Unsetenv, "RESET_BMC_PASSWORD")
	in, err := config.Decode([]byte(fmt.Sprintf("credentials:\n  a: {username: admin, passwordRef: {env: RESET_BMC_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{bmcEndpoint: 'https://192.0.2.3', credentialRef: a}]\n  dpus: [{bmcEndpoint: '%s', credentialRef: a}]\n  probes: {mode: inventory}\n", endpoint)), "/site.yaml")
	Expect(err).NotTo(HaveOccurred())
	request, err := config.Resolve(in)
	Expect(err).NotTo(HaveOccurred())
	return request.Targets[0]
}

var _ = Describe("fresh modern BF4 reset safety", func() {
	It("starts proof age before the first BMC request rather than refreshing it after collection", func() {
		fixture := newResetFixture()
		firstRequest := make(chan time.Time, 1)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case firstRequest <- time.Now():
			default:
			}
			fixture.handler(w, r)
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		target := resetFixtureTarget(server.URL)
		before := time.Now()
		observed, err := (Collector{}).ObserveResetSafety(ctx, target, time.Second, ResetSafetyRequest{Release: "26.8.0",
			SerialNumber: "SERIAL-A", PSID: "MT_0000001774", NotBefore: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(observed.ObservedAt.Before(before)).To(BeFalse())
		Expect(observed.ObservedAt.After(<-firstRequest)).To(BeFalse())
	})
	It("reuses authenticated identity and permits only owned session writes", func() {
		fixture := newResetFixture()
		server := httptest.NewTLSServer(http.HandlerFunc(fixture.handler))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observed, err := (Collector{}).ObserveResetSafety(ctx, resetFixtureTarget(server.URL), time.Second, ResetSafetyRequest{Release: "26.8.0",
			SerialNumber: "SERIAL-A", PSID: "MT_0000001774", NotBefore: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(observed.SerialNumber).To(Equal("SERIAL-A"))
		Expect(observed.TaskCount).To(Equal(1))
		Expect(observed.NICCopyState).To(Equal("NIC_COPY_STATE_NOT_EXPOSED"))
		Expect(fixture.writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
		data, err := json.Marshal(observed)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).NotTo(ContainSubstring("canary"))
	})

	DescribeTable("fails closed without accepting missing or contradictory physical facts", func(edit func(*resetFixture), code string) {
		fixture := newResetFixture()
		edit(fixture)
		server := httptest.NewTLSServer(http.HandlerFunc(fixture.handler))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observed, err := (Collector{}).ObserveResetSafety(ctx, resetFixtureTarget(server.URL), time.Second, ResetSafetyRequest{Release: "26.8.0",
			SerialNumber: "SERIAL-A", PSID: "MT_0000001774", NotBefore: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		})
		Expect(err).To(HaveOccurred())
		Expect(Code(err)).To(Equal(code))
		if strings.HasPrefix(code, "RESET_HEALTH") {
			Expect(Diagnostics(err)[0].Method).To(Equal("GET"))
			Expect(Diagnostics(err)[0].URI).NotTo(BeEmpty())
		}
		Expect(observed).To(BeZero())
		Expect(err.Error()).NotTo(ContainSubstring("canary"))
		Expect(fixture.writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
	},
		Entry("actual UEFI false-Off", func(f *resetFixture) {
			Object(f.docs[resetSystem], "BootProgress")["LastState"] = "PCIResourceConfigStarted"
		}, "RESET_ARM_NOT_OFF"),
		Entry("contradictory system rollup health", func(f *resetFixture) { Object(f.docs[resetSystem], "Status")["HealthRollup"] = "Critical" }, "RESET_HEALTH_ROLLUP_UNVERIFIED"),
		Entry("contradictory component health", func(f *resetFixture) {
			Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Status")["State"] = "Enabled"
		}, "RESET_HEALTH_UNVERIFIED"),
		Entry("malformed component health", func(f *resetFixture) {
			Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_ERoT_CPU_0"], "Status")["HealthRollup"] = nil
		}, "RESET_HEALTH_ROLLUP_UNVERIFIED"),
		Entry("contradictory update service health", func(f *resetFixture) { Object(f.docs["/redfish/v1/UpdateService"], "Status")["Health"] = "Critical" }, "RESET_HEALTH_UNVERIFIED"),
		Entry("On", func(f *resetFixture) { f.docs[resetSystem]["PowerState"] = "On" }, "RESET_ARM_NOT_OFF"),
		Entry("missing progress", func(f *resetFixture) { delete(f.docs[resetSystem], "BootProgress") }, "RESET_ARM_STATE_UNVERIFIED"),
		Entry("null progress", func(f *resetFixture) { f.docs[resetSystem]["BootProgress"] = nil }, "RESET_ARM_STATE_UNVERIFIED"),
		Entry("malformed progress", func(f *resetFixture) { Object(f.docs[resetSystem], "BootProgress")["LastState"] = []any{"None"} }, "RESET_ARM_STATE_UNVERIFIED"),
		Entry("earlier round", func(f *resetFixture) {
			Object(f.docs[resetSystem], "BootProgress")["LastStateTime"] = "2026-11-12T13:00:00Z"
		}, "RESET_ARM_STATE_UNVERIFIED"),
		Entry("future clock", func(f *resetFixture) {
			Object(f.docs[resetSystem], "BootProgress")["LastStateTime"] = "2099-01-01T00:00:00Z"
		}, "RESET_ARM_STATE_UNVERIFIED"),
		Entry("serial change", func(f *resetFixture) { f.docs[resetSystem]["SerialNumber"] = "OTHER" }, "CARD_IDENTITY_AMBIGUOUS"),
		Entry("PSID conflict", func(f *resetFixture) { f.docs[resetSystem]["AssetTag"] = "MT_OTHER" }, "RESET_PSID_CHANGED"),
		Entry("PSID absent", func(f *resetFixture) { delete(f.docs[resetSystem], "AssetTag") }, "RESET_PSID_UNVERIFIED"),
		Entry("no administrator", func(f *resetFixture) {
			f.docs["/redfish/v1/AccountService/Roles/Administrator"]["AssignedPrivileges"] = []any{"Login"}
		}, "RESET_BMC_ADMIN_UNVERIFIED"),
		Entry("task running", func(f *resetFixture) { f.docs["/redfish/v1/TaskService/Tasks/7"]["TaskState"] = "Running" }, "RESET_TASKS_NOT_IDLE"),
		Entry("legacy critical severity", func(f *resetFixture) {
			f.docs["/redfish/v1/TaskService/Tasks/7"]["Messages"] = []any{Document{"Severity": "Critical", "MessageSeverity": "OK"}}
		}, "RESET_TASKS_UNVERIFIED"),
		Entry("missing advertised task service", func(f *resetFixture) { delete(f.docs["/redfish/v1"], "Tasks") }, "UNSAFE_LINK"),
		Entry("malformed advertised task service link", func(f *resetFixture) {
			f.docs["/redfish/v1"]["Tasks"] = Document{"@odata.id": 7}
		}, "UNSAFE_LINK"),
		Entry("cross-origin advertised task service link", func(f *resetFixture) {
			f.docs["/redfish/v1"]["Tasks"] = Document{"@odata.id": "https://foreign.invalid/redfish/v1/TaskService"}
		}, "CROSS_ORIGIN_LINK"),
		Entry("path-traversing advertised task service link", func(f *resetFixture) {
			f.docs["/redfish/v1"]["Tasks"] = Document{"@odata.id": "/redfish/v1/../TaskService"}
		}, "UNSAFE_LINK"),
		Entry("partial tasks", func(f *resetFixture) { f.docs["/redfish/v1/TaskService/Tasks"]["Members@odata.count"] = 2 }, "INCOMPLETE_COLLECTION"),
		Entry("malformed task count cannot appear complete", func(f *resetFixture) { f.docs["/redfish/v1/TaskService/Tasks"]["Members@odata.count"] = "1" }, "INCOMPLETE_COLLECTION"),
		Entry("malformed task pagination cannot appear complete", func(f *resetFixture) { f.docs["/redfish/v1/TaskService/Tasks"]["Members@odata.nextLink"] = false }, "INVALID_RESPONSE"),
		Entry("malformed firmware pagination cannot appear complete", func(f *resetFixture) {
			f.docs["/redfish/v1/UpdateService/FirmwareInventory"]["Members@odata.nextLink"] = nil
		}, "INVALID_RESPONSE"),
		Entry("unknown inactive-only copy target", func(f *resetFixture) {
			uri := "/redfish/v1/UpdateService/FirmwareInventory/unknown"
			f.docs[uri] = Document{"@odata.id": uri, "Id": "Unknown", "Oem": Document{"Nvidia": Document{"InactiveFirmwareSlot": nil}}}
			collection := f.docs["/redfish/v1/UpdateService/FirmwareInventory"]
			collection["Members"] = append(collection["Members"].([]any), Document{"@odata.id": uri})
			collection["Members@odata.count"] = 11
		}, "RESET_FIRMWARE_STATE_UNVERIFIED"),
		Entry("duplicate pending identity", func(f *resetFixture) {
			uri := "/redfish/v1/UpdateService/FirmwareInventory/duplicate"
			f.docs[uri] = Document{"@odata.id": uri, "Id": "BlueField_FW_CPU_0_pending", "Version": ""}
			collection := f.docs["/redfish/v1/UpdateService/FirmwareInventory"]
			collection["Members"] = append(collection["Members"].([]any), Document{"@odata.id": uri})
			collection["Members@odata.count"] = 11
		}, "RESET_FIRMWARE_STATE_UNVERIFIED"),
		Entry("qualified CPU tuple on another BMC build", func(f *resetFixture) {
			d := f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_BMC_0"]
			d["Version"] = "BF4-26.11-0001"
			Object(Object(Object(d, "Oem"), "Nvidia"), "ActiveFirmwareSlot")["Version"] = "BF4-26.11-0001"
		}, "RESET_PLATFORM_UNQUALIFIED"),
		Entry("management card chassis unhealthy", func(f *resetFixture) { Object(f.docs["/redfish/v1/Chassis/card"], "Status")["Health"] = "Critical" }, "RESET_CARD_CHASSIS_UNVERIFIED"),
		Entry("management card chassis not an Arm-off signal", func(f *resetFixture) { f.docs["/redfish/v1/Chassis/card"]["PowerState"] = "Off" }, "RESET_CARD_CHASSIS_UNVERIFIED"),
		Entry("CPU offline rollup near miss", func(f *resetFixture) {
			Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Status")["HealthRollup"] = "Critical"
		}, "RESET_CPU_OFF_STATE_UNQUALIFIED"),
		Entry("CPU offline Conditions absent", func(f *resetFixture) {
			delete(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Status"), "Conditions")
		}, "RESET_CPU_OFF_STATE_UNQUALIFIED"),
		Entry("CPU offline Conditions malformed", func(f *resetFixture) {
			Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Status")["Conditions"] = nil
		}, "RESET_CPU_OFF_STATE_UNQUALIFIED"),
		Entry("CPU offline Conditions nonempty", func(f *resetFixture) {
			Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Status")["Conditions"] = []any{Document{"MessageId": "fault"}}
		}, "RESET_CPU_OFF_STATE_UNQUALIFIED"),
		Entry("CPU offline copy in progress", func(f *resetFixture) {
			Object(Object(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Oem"), "Nvidia"), "InactiveFirmwareSlot")["FirmwareState"] = "Copying"
		}, "RESET_FIRMWARE_NOT_IDLE"),
		Entry("CPU offline backup version mismatch", func(f *resetFixture) {
			Object(Object(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Oem"), "Nvidia"), "InactiveFirmwareSlot")["Version"] = "26.01-6"
		}, "RESET_CPU_OFF_STATE_UNQUALIFIED"),
		Entry("CPU offline build near miss", func(f *resetFixture) {
			Object(Object(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_CPU_0"], "Oem"), "Nvidia"), "InactiveFirmwareSlot")["BuildType"] = "Development"
		}, "RESET_FIRMWARE_NOT_IDLE"),
		Entry("other component cannot use CPU offline tuple", func(f *resetFixture) {
			f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_ERoT_CPU_0"]["Status"] = Document{"State": "StandbyOffline", "Health": "Critical", "HealthRollup": "OK", "Conditions": []any{}}
		}, "RESET_HEALTH_UNVERIFIED"),
		Entry("board copy busy", func(f *resetFixture) {
			Object(Object(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_ERoT_CPU_0"], "Oem"), "Nvidia"), "InactiveFirmwareSlot")["FirmwareState"] = "Copying"
		}, "RESET_FIRMWARE_NOT_IDLE"),
		Entry("NIC representation unqualified", func(f *resetFixture) {
			Object(Object(Object(f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_NIC_0"], "Oem"), "Nvidia"), "ActiveFirmwareSlot")["BuildType"] = "Unknown"
		}, "RESET_NIC_COPY_STATE_UNQUALIFIED"),
		Entry("pending firmware", func(f *resetFixture) {
			f.docs["/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_NIC_0_pending"]["Version"] = "82.48.4044"
		}, "RESET_FIRMWARE_NOT_IDLE"),
		Entry("cleanup denied", func(f *resetFixture) { f.cleanupFail = true }, "ACCESS_DENIED"),
		Entry("state changes during observation", func(f *resetFixture) {
			f.mutate = func(uri string, n int, d Document) {
				if uri == resetSystem && n == 2 {
					Object(d, "BootProgress")["LastStateTime"] = "2026-11-13T13:00:01Z"
				}
			}
		}, "RESET_SAFETY_STATE_CHANGED"),
	)
})

var _ = Describe("complete collection nextLink typing", func() {
	It("rejects malformed nextLink even when the first page count appears complete", func() {
		for _, next := range []any{nil, false, float64(1), []any{}, Document{}} {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(Document{"Members": []any{}, "Members@odata.count": 0, "Members@odata.nextLink": next})
			}))
			client := &Client{origin: server.URL, http: server.Client()}
			_, err := client.Collection(context.Background(), "/redfish/v1/Systems")
			Expect(Code(err)).To(Equal("INVALID_RESPONSE"))
			server.Close()
		}
	})
	It("accepts explicitly empty nextLink", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(Document{"Members": []any{}, "Members@odata.count": 0, "Members@odata.nextLink": ""})
		}))
		defer server.Close()
		client := &Client{origin: server.URL, http: server.Client()}
		members, err := client.Collection(context.Background(), "/redfish/v1/Systems")
		Expect(err).NotTo(HaveOccurred())
		Expect(members).To(BeEmpty())
	})
})

var _ = Describe("reset safety cancellation", func() {
	It("returns no safety receipt and closes only its owned session after cancellation", func() {
		fixture := newResetFixture()
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redfish/v1/TaskService/Tasks/7" {
				<-r.Context().Done()
				return
			}
			fixture.handler(w, r)
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		observed, err := (Collector{}).ObserveResetSafety(ctx, resetFixtureTarget(server.URL), time.Second, ResetSafetyRequest{Release: "26.8.0", SerialNumber: "SERIAL-A", PSID: "MT_0000001774", NotBefore: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)})
		Expect(err).To(HaveOccurred())
		Expect(observed).To(BeZero())
		Expect(fixture.writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
	})
})
