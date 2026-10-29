// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"testing"
	"time"
)

func TestDiscovery(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF discovery checks") }
func estate() inventory.Inventory {
	return inventory.Inventory{Collection: inventory.Collection{Mode: "inventory"}, Management: &inventory.Management{APIEndpoint: "https://api.invalid", SystemNamespaceUID: "cluster-a", TrustDigest: "system-roots", KubernetesVersion: "v1", Nodes: []inventory.Node{{Name: "cp", UID: "node-a", Ready: true}}}, RequestedScope: []inventory.ScopeEndpoint{}, DPUs: []inventory.Device{{ID: "dpu-a", RecordKey: "a", SerialNumber: "CARD-A", ObservationState: "observed"}, {ID: "dpu-b", RecordKey: "b", SerialNumber: "CARD-B", ObservationState: "observed"}}, Hosts: []inventory.Device{{ID: "host-a", RecordKey: "host", Adapters: []inventory.Adapter{{Key: "adapter-a", SerialNumber: "CARD-A"}, {Key: "adapter-b", SerialNumber: "CARD-B"}, {Key: "adapter-c", SerialNumber: "CARD-C", Selection: "not-selected"}}}}, Associations: []inventory.Association{}}
}

var _ = Describe("inventory semantics", func() {
	It("maps two selected cards to one host without selecting the extra or mutating input", func() {
		v := estate()
		before, _ := json.Marshal(v.Hosts)
		a, c := Correlate(v.DPUs, v.Hosts)
		Expect(a).To(HaveLen(2))
		for _, row := range c {
			Expect(row.State).To(Equal("passed"))
		}
		after, _ := json.Marshal(v.Hosts)
		Expect(after).To(Equal(before))
		Expect(v.Hosts[0].Adapters[2].Selection).To(Equal("not-selected"))
	})
	It("rejects duplicate endpoint physical identity and ambiguous host matches", func() {
		v := estate()
		v.Hosts = append(v.Hosts, v.Hosts[0])
		a, c := Correlate(v.DPUs, v.Hosts)
		Expect(a).To(BeEmpty())
		Expect(c[0].State).To(Equal("failed"))
	})
	It("keeps fingerprints stable across order/time/readiness and changes them for identity", func() {
		a := estate()
		Canonicalize(&a)
		first := a.Fingerprint
		a.Collection.StartedAt = time.Now()
		a.Management.Nodes[0].Ready = false
		a.DPUs[0], a.DPUs[1] = a.DPUs[1], a.DPUs[0]
		Canonicalize(&a)
		Expect(a.Fingerprint).To(Equal(first))
		a.DPUs[0].ID = "replacement"
		Canonicalize(&a)
		Expect(a.Fingerprint).NotTo(Equal(first))
	})
	It("blocks endpoint replacement but preserves unknown observations", func() {
		old := estate()
		cur := estate()
		cur.DPUs[0].ID = "replacement"
		Expect(inventory.IdentityConflict(&old, &cur)).To(BeTrue())
		cur.DPUs[0].ID = ""
		Expect(inventory.IdentityConflict(&old, &cur)).To(BeFalse())
		Expect(Diff(nil, &cur)[0].State).To(Equal("baseline-absent"))
	})
	It("applies error precedence without hiding inspection failures", func() {
		r := NewReport()
		r.Discovery.Outcome = "partial"
		Expect(ExitCode(r)).To(Equal(2))
		r.Errors = []inventory.Problem{{ExitCode: 2}, {ExitCode: 3}}
		Expect(ExitCode(r)).To(Equal(3))
		r.Errors = append(r.Errors, inventory.Problem{ExitCode: 1})
		Expect(ExitCode(r)).To(Equal(1))
	})
})

var _ = Describe("refresh regressions", func() {
	It("canonicalizes nested network ordering without changing the observations", func() {
		a := estate()
		a.Management.Nodes[0].Interfaces = []inventory.Interface{{Index: 2, Name: "b", Addresses: []string{"192.0.2.2/24", "192.0.2.1/24"}}, {Index: 1, Name: "a"}}
		a.Management.Nodes[0].Routes = []inventory.Route{{Destination: "b", Table: 2}, {Destination: "a", Table: 1}}
		Canonicalize(&a)
		original := a.Fingerprint
		b := estate()
		b.Management.Nodes[0].Interfaces = []inventory.Interface{{Index: 1, Name: "a"}, {Index: 2, Name: "b", Addresses: []string{"192.0.2.1/24", "192.0.2.2/24"}}}
		b.Management.Nodes[0].Routes = []inventory.Route{{Destination: "a", Table: 1}, {Destination: "b", Table: 2}}
		Canonicalize(&b)
		Expect(b.Fingerprint).To(Equal(original))
		Expect(a.Management.Nodes[0].Interfaces[0].Name).To(Equal("b"))
	})
	It("classifies failed current identity observations as unknown", func() {
		old := estate()
		current := estate()
		current.DPUs[0].ObservationState = "unavailable"
		current.DPUs[0].ID = ""
		changes := Diff(&old, &current)
		Expect(changes).NotTo(BeEmpty())
		for _, change := range changes {
			if change.Path == "device.a.identity" {
				Expect(change.State).To(Equal("unknown"))
			}
		}
	})
})

var _ = Describe("scope evidence on refresh", func() {
	It("keeps missing scope evidence separate from a proven replacement", func() {
		old := estate()
		Canonicalize(&old)
		current := estate()
		current.Management = nil
		Canonicalize(&current)
		Expect(current.ScopeDigest).To(BeEmpty())
		Expect(scopeConflict(&old, &current)).To(BeFalse())
		current = estate()
		current.Management.SystemNamespaceUID = "replacement"
		Canonicalize(&current)
		Expect(scopeConflict(&old, &current)).To(BeTrue())
	})
})
