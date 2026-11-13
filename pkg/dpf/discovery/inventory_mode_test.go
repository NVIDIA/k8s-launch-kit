// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fixtureCollector struct {
	fail   bool
	admin  bool
	cancel context.CancelFunc
}

func (f fixtureCollector) Collect(_ context.Context, t config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	d := inventory.Device{ID: "host-one", RecordKey: t.InputRef, InputRef: t.InputRef, ObservationState: "observed", Qualification: "unqualified", InventoryCompleteness: "partial", EvidenceRefs: []string{"source"}}
	if t.SSH != nil {
		d.SSHEndpoint = t.Endpoint
	} else {
		d.BMCEndpoint = t.Endpoint
	}
	if t.Role == "hosts" {
		d.Adapters = []inventory.Adapter{{Key: "card-one", IdentityState: "known", SerialNumber: "CARD-ONE", Model: "BlueField-4", EvidenceRefs: []string{"source"}}}
	} else {
		d.ID = "dpu-one"
		d.SerialNumber = "CARD-ONE"
		if t.SSH != nil {
			d.SSHEndpoint = t.Endpoint
		} else {
			d.BMCEndpoint = t.Endpoint
		}
		if f.admin {
			d.Observations = map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Message: "fixture verified"}}
		}
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
	It("completes without Kubernetes when DPU BMC administrator privileges are verified, retaining qualification and readiness gaps", func() {
		req := inventoryRequest()
		req.Targets[1].SSH = nil
		req.Targets[1].Endpoint = "https://dpu.invalid"
		r := Collect(context.Background(), req, "run", Dependencies{BMC: fixtureCollector{admin: true}})
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
		Expect(ExitCode(r)).To(Equal(2))
		Expect(r.Discovery.Outcome).To(Equal("partial"))
		Expect(r.Inventory.DPUs).To(HaveLen(1))
		Expect(r.Inventory.Associations).To(HaveLen(1))
		Expect(r.Inventory.DPUs[0].BMCEndpoint).To(BeEmpty())
		Expect(r.Inventory.DPUs[0].SSHEndpoint).To(BeEmpty())
		Expect(r.Inventory.DPUs[0].ReaderAdapter).To(Equal("host-card-v1"))
	})
	It("fails closed for ARM SSH even when the collector claims admin or reports a local BMC address", func() {
		r := Collect(context.Background(), inventoryRequest(), "run", Dependencies{BMC: fixtureCollector{}, SSH: fixtureCollector{admin: true}})
		Expect(ExitCode(r)).To(Equal(2))
		Expect(r.Discovery.Outcome).To(Equal("partial"))
		Expect(r.Inventory.Associations).To(HaveLen(1))
		Expect(r.Discovery.Checks).To(ContainElement(And(HaveField("ID", "dpf.management.bmc-administrator"), HaveField("State", "failed"), HaveField("RequiredPhase", "discover"))))
	})
	It("publishes completed admin-verified inventory and preserves its handoff after missing admin evidence", func() {
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		seed := []byte("credentials:\n  read: {username: reader, passwordRef: {env: DPF_PUBLICATION_PASSWORD}}\ndiscovery:\n  hosts: [{bmcEndpoint: 'https://host.invalid', credentialRef: read}]\n  dpus: [{bmcEndpoint: 'https://dpu.invalid', credentialRef: read}]\n  probes: {mode: inventory}\n")
		Expect(os.WriteFile(filepath.Join(dir, "seed.yaml"), seed, 0600)).To(Succeed())
		input, err := config.Load(filepath.Join(dir, "seed.yaml"))
		Expect(err).NotTo(HaveOccurred())
		req := inventoryRequest()
		req.Input = input
		req.Targets[1].SSH = nil
		req.Targets[1].Endpoint = "https://dpu.invalid"
		output := filepath.Join(dir, "discovered.yaml")
		for _, verified := range []bool{true, false} {
			store, err := artifacts.New(input, output, true)
			Expect(err).NotTo(HaveOccurred())
			report := Collect(context.Background(), req, store.RunID(), Dependencies{BMC: fixtureCollector{admin: verified}})
			Expect(store.Publish(context.Background(), &report)).To(Succeed())
			if verified {
				Expect(ExitCode(report)).To(Equal(0))
			} else {
				Expect(ExitCode(report)).To(Equal(2))
				Expect(report.Artifacts).To(ContainElement(HaveField("Path", filepath.Join(store.RunDir, "partial.yaml"))))
				partial, err := os.ReadFile(filepath.Join(store.RunDir, "partial.yaml"))
				Expect(err).NotTo(HaveOccurred())
				Expect(string(partial)).To(ContainSubstring("BMC_ADMIN_NOT_VERIFIED"))
			}
			saved, err := config.Load(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(saved.Site.Inventory.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
		}
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
		Expect(ExitCode(first)).To(Equal(2))
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
