// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"testing"
	"time"
)

func TestDiscovery(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF discovery checks") }
func estate() inventory.Inventory {
	return inventory.Inventory{Collection: inventory.Collection{Mode: "inventory"}, Management: &inventory.Management{APIEndpoint: "https://api.invalid", SystemNamespaceUID: "cluster-a", TrustDigest: "system-roots", KubernetesVersion: "v1", Nodes: []inventory.Node{{Name: "cp", UID: "node-a", Ready: true}}}, RequestedScope: []inventory.ScopeEndpoint{}, DPUs: []inventory.Device{{ID: "dpu-a", RecordKey: "a", SerialNumber: "CARD-A", ObservationState: "observed"}, {ID: "dpu-b", RecordKey: "b", SerialNumber: "CARD-B", ObservationState: "observed"}}, Hosts: []inventory.Device{{ObservationState: "observed", ID: "host-a", RecordKey: "host", Adapters: []inventory.Adapter{{Key: "adapter-a", SerialNumber: "CARD-A"}, {Key: "adapter-b", SerialNumber: "CARD-B"}, {Key: "adapter-c", SerialNumber: "CARD-C", Selection: "not-selected"}}}}, Associations: []inventory.Association{}}
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
	It("rejects changed or legacy Redfish trust policies without calling unread observations replacements", func() {
		for _, prior := range []string{"", "system-roots", "custom-ca-before", "verification-disabled"} {
			old := estate()
			old.RequestedScope = []inventory.ScopeEndpoint{{Role: "dpus", Endpoint: "https://192.0.2.1", TLSTrustDigest: prior}}
			Canonicalize(&old)
			current := estate()
			current.RequestedScope = []inventory.ScopeEndpoint{{Role: "dpus", Endpoint: "https://192.0.2.1", TLSTrustDigest: "custom-ca-after"}}
			Canonicalize(&current)
			Expect(scopeConflict(&old, &current)).To(BeTrue())
			Expect(inventory.IdentityConflict(&old, &current)).To(BeFalse())
			current.ScopeDigest = "" // Required management observation failed, so no complete scope exists.
			Expect(scopeConflict(&old, &current)).To(BeFalse())
		}
		old := estate()
		old.RequestedScope = []inventory.ScopeEndpoint{{Role: "hosts", Endpoint: "https://192.0.2.2", TLSTrustDigest: "system-roots"}}
		Canonicalize(&old)
		current := old
		Canonicalize(&current)
		Expect(scopeConflict(&old, &current)).To(BeFalse())
	})
	It("records the resolved TLS policy even when collection fails before remote access", func() {
		Expect(os.Setenv("DPF_SCOPE_TEST_PASSWORD", "private-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_SCOPE_TEST_PASSWORD")
		input, err := config.Decode([]byte(`management:
  kubeconfigRef: {file: missing-kubeconfig}
credentials:
  reader: {username: reader, passwordRef: {env: DPF_SCOPE_TEST_PASSWORD}}
discovery:
  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: reader, tls: {insecureSkipVerify: true}}]
  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: reader}]
  probes: {mode: inventory}
`), GinkgoT().TempDir()+"/site.yaml")
		Expect(err).NotTo(HaveOccurred())
		req, err := config.Resolve(input)
		Expect(err).NotTo(HaveOccurred())
		report := Collect(context.Background(), req, "test", Dependencies{})
		Expect(report.Inventory.RequestedScope).To(HaveLen(2))
		Expect(report.Inventory.RequestedScope[0].TLSInsecureSkipVerify).To(BeTrue())
		Expect(report.Inventory.RequestedScope[1].TLSInsecureSkipVerify).To(BeFalse())
		Expect(report.Errors[0].Code).To(Equal("INVALID_MANAGEMENT_ACCESS"))
	})
	It("retains insecure BMC policy in saved scope and rejects a silent trust-policy change", func() {
		old := estate()
		old.RequestedScope = []inventory.ScopeEndpoint{{Role: "dpus", Endpoint: "https://192.0.2.1", TLSInsecureSkipVerify: false}}
		Canonicalize(&old)
		current := estate()
		current.RequestedScope = []inventory.ScopeEndpoint{{Role: "dpus", Endpoint: "https://192.0.2.1", TLSInsecureSkipVerify: true}}
		Canonicalize(&current)
		Expect(scopeConflict(&old, &current)).To(BeTrue())
		data, err := json.Marshal(current)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring(`"tlsInsecureSkipVerify":true`))
	})
	It("distinguishes infrastructure-only scope from management scope", func() {
		old := estate()
		Canonicalize(&old)
		current := estate()
		current.Management = nil
		Canonicalize(&current)
		Expect(current.ScopeDigest).NotTo(BeEmpty())
		Expect(scopeConflict(&old, &current)).To(BeTrue())
		current = estate()
		current.Management.SystemNamespaceUID = "replacement"
		Canonicalize(&current)
		Expect(scopeConflict(&old, &current)).To(BeTrue())
	})
})
