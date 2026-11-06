// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fixtureCollector struct {
	fail   bool
	cancel context.CancelFunc
}

func (f fixtureCollector) Collect(_ context.Context, t config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	d := inventory.Device{ID: "host-one", RecordKey: t.InputRef, InputRef: t.InputRef, ObservationState: "observed", Qualification: "unqualified", InventoryCompleteness: "partial", EvidenceRefs: []string{"source"}}
	if t.Role == "hosts" {
		d.Adapters = []inventory.Adapter{{Key: "card-one", SerialNumber: "CARD-ONE", Model: "BlueField-4", EvidenceRefs: []string{"source"}}}
	} else {
		d.ID = "dpu-one"
		d.SerialNumber = "CARD-ONE"
		d.SSHEndpoint = t.Endpoint
	}
	if f.fail {
		d.ObservationState = "unavailable"
		d.ReasonCode = "CLEANUP_INCOMPLETE"
		return d, nil, &redfish.ObservationError{Primary: &redfish.Error{Code: "ACCESS_DENIED", URI: "/redfish/v1/Systems", Status: 403, Method: "GET"}, Cleanup: &redfish.Error{Code: "ACCESS_DENIED", URI: "/redfish/v1/SessionService/Sessions/own", Status: 403, Method: "DELETE"}, SessionURI: "/redfish/v1/SessionService/Sessions/own"}
	}
	if f.cancel != nil {
		f.cancel()
	}
	return d, map[string]inventory.Evidence{"source": {Kind: "fixture", Authenticated: true}}, nil
}
func inventoryRequest() config.Request {
	return config.Request{Mode: "inventory", Timeout: time.Second, RequestTimeout: time.Second, MaxConcurrentBMCs: 2, Targets: []config.Target{{Role: "hosts", Endpoint: "https://host.invalid", InputRef: "/discovery/hosts/0"}, {Role: "dpus", Endpoint: "ssh://arm.invalid:22", InputRef: "/discovery/dpus/0", SSH: &config.SSHAccess{}}}}
}

var _ = Describe("independent inventory acceptance", func() {
	It("completes identity and mapping without Kubernetes or DPU Redfish, retaining qualification and readiness gaps", func() {
		r := Collect(context.Background(), inventoryRequest(), "run", Dependencies{BMC: fixtureCollector{}, SSH: fixtureCollector{}})
		Expect(ExitCode(r)).To(Equal(0))
		Expect(r.Inventory.Management).To(BeNil())
		Expect(r.Inventory.Associations).To(HaveLen(1))
		Expect(r.Acceptance).To(Equal("not-evaluated"))
		Expect(r.Inventory.ScopeDigest).NotTo(BeEmpty())
		Expect(r.Discovery.Checks).To(ContainElement(And(HaveField("ID", "dpf.device.qualification"), HaveField("State", "failed"), HaveField("RequiredPhase", "generate"))))
		Expect(r.Discovery.Checks).To(ContainElement(And(HaveField("ID", "dpf.management.dpf-authentication"), HaveField("State", "not-evaluated"), HaveField("RequiredPhase", "deploy"))))
	})
	It("derives only host-reported card identity without inventing a BMC or ARM binding", func() {
		req := inventoryRequest()
		req.Targets = req.Targets[:1]
		r := Collect(context.Background(), req, "run", Dependencies{BMC: fixtureCollector{}})
		Expect(ExitCode(r)).To(Equal(0))
		Expect(r.Inventory.DPUs).To(HaveLen(1))
		Expect(r.Inventory.Associations).To(HaveLen(1))
		Expect(r.Inventory.DPUs[0].BMCEndpoint).To(BeEmpty())
		Expect(r.Inventory.DPUs[0].SSHEndpoint).To(BeEmpty())
		Expect(r.Inventory.DPUs[0].ReaderAdapter).To(Equal("host-card-v1"))
	})
	It("retains observation and cleanup failures instead of substituting another source", func() {
		r := Collect(context.Background(), inventoryRequest(), "run", Dependencies{BMC: fixtureCollector{fail: true}, SSH: fixtureCollector{}})
		Expect(ExitCode(r)).To(Equal(3))
		Expect(r.Inventory.Associations).To(BeEmpty())
		Expect(r.Discovery.Cleanup.State).To(Equal("incomplete"))
		Expect(r.Errors).To(ContainElement(And(HaveField("HTTPStatus", 403), HaveField("Method", "GET"), HaveField("Stage", "observation"))))
		Expect(r.Errors).To(ContainElement(And(HaveField("HTTPStatus", 403), HaveField("Method", "DELETE"), HaveField("Stage", "cleanup"))))
		Expect(r.Discovery.Cleanup.Leftovers).To(ContainElement(ContainSubstring("/Sessions/own")))
	})
	It("requires a fresh seed for legacy serial-keyed host cards without rejecting stable or explicit bindings", func() {
		req := inventoryRequest()
		req.Targets = req.Targets[:1]
		first := Collect(context.Background(), req, "first", Dependencies{BMC: fixtureCollector{}})
		Expect(ExitCode(first)).To(Equal(0))
		Expect(inventory.LegacyHostCardBindings(first.Inventory)).To(BeFalse())
		host := &first.Inventory.Hosts[0]
		host.Adapters[0].Key = inventory.Digest("dpf-adapter-v1", []string{host.ID, host.Adapters[0].SerialNumber})
		first.Inventory.DPUs = HostCards(first.Inventory.Hosts)
		Expect(inventory.LegacyHostCardBindings(first.Inventory)).To(BeTrue())
		req.Input.Site.Inventory = first.Inventory
		current := Collect(context.Background(), req, "second", Dependencies{BMC: fixtureCollector{}})
		Expect(current.Discovery.Outcome).To(Equal("partial"))
		Expect(current.Errors).To(ContainElement(And(HaveField("Code", "SCOPE_CONFLICT"), HaveField("Message", ContainSubstring("legacy Redfish card bindings")))))
		first.Inventory.DPUs[0].ReaderAdapter = "explicit-endpoint"
		Expect(inventory.LegacyHostCardBindings(first.Inventory)).To(BeFalse())
		first.Inventory.DPUs = HostCards(first.Inventory.Hosts)
		host.Adapters[0].SerialNumber = ""
		Expect(inventory.LegacyHostCardBindings(first.Inventory)).To(BeFalse())
	})
	It("does not leave a completed handoff after cancellation of otherwise valid observations", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := inventoryRequest()
		req.Targets = req.Targets[:1]
		r := Collect(ctx, req, "run", Dependencies{BMC: fixtureCollector{cancel: cancel}})
		Expect(r.Inventory.Associations).To(HaveLen(1))
		Expect(r.Discovery.Outcome).To(Equal("partial"))
		Expect(r.Execution).To(Equal("cancelled"))
		Expect(ExitCode(r)).To(Equal(1))
	})

	It("keeps unknown-serial BlueFields and rejects fabricated or ambiguous associations", func() {
		h := inventory.Device{ID: "host", RecordKey: "h", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "one", Model: "BlueField-4"}}}
		d := HostCards([]inventory.Device{h})
		Expect(d).To(HaveLen(1))
		Expect(d[0].ID).To(BeEmpty())
		a, c := Correlate(d, []inventory.Device{h})
		Expect(a).To(BeEmpty())
		Expect(c[0].State).To(Equal("failed"))
		h.Adapters[0].SerialNumber = "SAME"
		h.Adapters = append(h.Adapters, inventory.Adapter{Key: "two", Model: "BlueField-4", SerialNumber: "SAME"})
		a, c = Correlate(HostCards([]inventory.Device{h}), []inventory.Device{h})
		Expect(a).To(BeEmpty())
		Expect(c[0].State).To(Equal("failed"))
	})
})
