// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/addresslookup"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func TestDiscovery(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF discovery checks") }
func estate() inventory.Inventory {
	return inventory.Inventory{Collection: inventory.Collection{Mode: "inventory"}, Management: &inventory.Management{APIEndpoint: "https://api.invalid", SystemNamespaceUID: "cluster-a", TrustDigest: "system-roots", KubernetesVersion: "v1", Nodes: []inventory.Node{{Name: "cp", UID: "node-a", Ready: true}}}, RequestedScope: []inventory.ScopeEndpoint{}, DPUs: []inventory.Device{{ID: "dpu-a", BMCEndpoint: "https://dpu-a.invalid", Model: "B4240", ReaderAdapter: "bluefield-card-v1", RecordKey: "a", SerialNumber: "CARD-A", ObservationState: "observed"}, {ID: "dpu-b", BMCEndpoint: "https://dpu-b.invalid", Model: "B4240", ReaderAdapter: "bluefield-card-v1", RecordKey: "b", SerialNumber: "CARD-B", ObservationState: "observed"}}, Hosts: []inventory.Device{{ObservationState: "observed", ID: "host-a", BMCEndpoint: "https://host.invalid", RecordKey: "host", Adapters: []inventory.Adapter{{Key: "adapter-a", SerialNumber: "CARD-A"}, {Key: "adapter-b", SerialNumber: "CARD-B"}, {Key: "adapter-c", SerialNumber: "CARD-C", Selection: "not-selected"}}}}, Associations: []inventory.Association{}}
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
	It("accepts NVIDIA or unknown adapter vendors without filtering mixed host inventory", func() {
		for _, vendor := range []string{"15b3", ""} {
			v := estate()
			v.Hosts[0].Adapters[0].VendorID = vendor
			v.Hosts[0].Adapters[2].VendorID = "8086"
			before, err := json.Marshal(v.Hosts)
			Expect(err).NotTo(HaveOccurred())
			associations, checks := Correlate(v.DPUs, v.Hosts)
			Expect(associations).To(HaveLen(2))
			Expect(checks).To(ConsistOf(HaveField("State", "passed"), HaveField("State", "passed")))
			after, err := json.Marshal(v.Hosts)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
		}
	})
	It("rejects a same-serial PCI vendor contradiction with both source evidence references", func() {
		v := estate()
		v.DPUs = v.DPUs[:1]
		v.DPUs[0].EvidenceRefs = []string{"dpu-serial"}
		v.Hosts[0].Adapters[0].VendorID = "8086"
		v.Hosts[0].Adapters[0].EvidenceRefs = []string{"host-serial", "host-pci-vendor"}
		before, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		associations, checks := Correlate(v.DPUs, v.Hosts)
		Expect(associations).To(BeEmpty())
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].State).To(Equal("failed"))
		Expect(checks[0].ReasonCode).To(Equal("HOST_ADAPTER_VENDOR_CONFLICT"))
		Expect(checks[0].EvidenceRefs).To(ConsistOf("dpu-serial", "host-serial", "host-pci-vendor"))
		ExplainCheck(&checks[0])
		Expect(checks[0].Message).To(ContainSubstring("Verify the selected endpoints"))
		after, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})
	It("does not discard a contradictory candidate to accept another same-serial NVIDIA card", func() {
		for _, reverse := range []bool{false, true} {
			v := estate()
			v.DPUs = v.DPUs[:1]
			v.Hosts[0].Adapters[0].VendorID = "8086"
			other := inventory.Device{ID: "host-b", RecordKey: "other-host", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "nvidia-card", SerialNumber: "CARD-A", VendorID: "15b3"}}}
			if reverse {
				v.Hosts = append([]inventory.Device{other}, v.Hosts...)
			} else {
				v.Hosts = append(v.Hosts, other)
			}
			associations, checks := Correlate(v.DPUs, v.Hosts)
			Expect(associations).To(BeEmpty())
			Expect(checks[0].ReasonCode).To(Equal("HOST_ADAPTER_VENDOR_CONFLICT"))
			Expect(checks[0].State).To(Equal("failed"))
		}
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
	It("does not treat removal of legacy BMC metadata as a material or scope change", func() {
		before, after := estate(), estate()
		before.Evidence = map[string]inventory.Evidence{"old-bmc-ref": {Kind: "redfish", Authenticated: true}}
		before.DPUs[0].EvidenceRefs = []string{"old-bmc-ref"}
		before.Hosts[0].EvidenceRefs = []string{"old-bmc-ref"}
		before.Hosts[0].Adapters[0].EvidenceRefs = []string{"old-bmc-ref"}
		before.DPUs[0].Firmware = []inventory.Firmware{{Component: "bmc", State: "known", RawVersion: "1.0", Activation: "unknown", EvidenceRefs: []string{"old-bmc-ref"}}}
		after.DPUs[0].Firmware = []inventory.Firmware{{Component: "bmc", State: "known", RawVersion: "1.0", Activation: "unknown"}}
		before.Associations, _ = Correlate(before.DPUs, before.Hosts)
		after.Associations, _ = Correlate(after.DPUs, after.Hosts)
		Canonicalize(&before)
		Canonicalize(&after)
		Expect(after.Fingerprint).To(Equal(before.Fingerprint))
		Expect(after.ScopeDigest).To(Equal(before.ScopeDigest))
		Expect(Diff(&before, &after)).To(BeEmpty())
	})
	It("keeps an unchanged SSH optional-read failure stable through v3 save and reload", func() {
		before := estate()
		before.Management.Nodes[0].Addresses = []string{}
		before.Hosts[0].SSHEndpoint = "ssh://192.0.2.10:22"
		before.Hosts[0].ReaderAdapter = "linux-host-ssh-v1"
		before.Hosts[0].Runtime = &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{
			"routes": {State: "unavailable", Values: []string{}, ReasonCode: "COMMAND_FAILED", Message: "ip route failed", Tool: "ip"},
		}}
		Canonicalize(&before)

		encoded, err := yaml.Marshal(before)
		Expect(err).NotTo(HaveOccurred())
		var after inventory.Inventory
		Expect(yaml.Unmarshal(encoded, &after)).To(Succeed())
		Canonicalize(&after)

		failure := after.Hosts[0].Runtime.Observations["routes"]
		Expect(failure.State).To(Equal("unavailable"))
		Expect(failure.Values).To(BeEmpty())
		Expect(failure.ReasonCode).To(Equal("COMMAND_FAILED"))
		Expect(failure.Message).To(Equal("ip route failed"))
		Expect(failure.Tool).To(Equal("ip"))
		Expect(Diff(&before, &after)).To(BeEmpty())
		Expect(after.Fingerprint).To(Equal(before.Fingerprint))
	})
	It("retains SSH host references when correlating with a BMC DPU without metadata", func() {
		v := estate()
		v.Hosts[0].SSHEndpoint = "ssh://192.0.2.1:22"
		v.Hosts[0].Adapters[0].EvidenceRefs = []string{"ssh-host-card"}
		v.DPUs[0].BMCEndpoint = "https://192.0.2.2"
		associations, checks := Correlate(v.DPUs, v.Hosts)
		Expect(associations).To(ContainElement(And(HaveField("DPUID", "dpu-a"), HaveField("EvidenceRefs", []string{"ssh-host-card"}))))
		Expect(checks).To(ContainElement(And(HaveField("Subject", "a"), HaveField("State", "passed"))))
	})

	It("fingerprints Redfish PSID values and eligibility without diagnostic prose", func() {
		before, after := estate(), estate()
		before.DPUs[0].Observations = map[string]inventory.Observation{"psid": {State: "observed", Values: []string{"MT_0000001774"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish", Message: "initial"}}
		after.DPUs[0].Observations = map[string]inventory.Observation{"psid": {State: "observed", Values: []string{"MT_0000001774"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish", Message: "different prose"}}
		Canonicalize(&before)
		Canonicalize(&after)
		Expect(after.Fingerprint).To(Equal(before.Fingerprint))
		after.DPUs[0].Observations["psid"] = inventory.Observation{State: "observed", Values: []string{"MT_0000001775"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"}
		Canonicalize(&after)
		Expect(after.Fingerprint).NotTo(Equal(before.Fingerprint))
		after.DPUs[0].Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: "PSID_CONFLICT", Tool: "redfish"}
		Canonicalize(&after)
		Expect(after.Fingerprint).NotTo(Equal(before.Fingerprint))
		Expect(Diff(&before, &after)).To(ContainElement(HaveField("Path", ContainSubstring(".psid"))))
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
	It("keeps address lookup material stable after YAML removes only provenance references", func() {
		before := estate()
		before.AddressLookup = &inventory.AddressLookup{Candidates: []inventory.AddressCandidate{{
			Name: "dpu-a", Role: "dpus", MAC: "02:00:00:00:00:01", Address: "192.0.2.44", Source: "neighbor-cache", EvidenceRef: "transient-probe-1",
		}}, Observations: map[string]inventory.Observation{
			"source.0":     {State: "observed", Values: []string{"source-bytes-digest-old"}},
			"device.dpu-a": {State: "candidate", Values: []string{"192.0.2.44"}, ReasonCode: "ADDRESS_UNVERIFIED", Message: "verify candidate"},
		}}
		originalLookup, err := json.Marshal(before.AddressLookup)
		Expect(err).NotTo(HaveOccurred())
		Canonicalize(&before)
		original, err := json.Marshal(before)
		Expect(err).NotTo(HaveOccurred())
		saved, err := yaml.Marshal(before)
		Expect(err).NotTo(HaveOccurred())
		var reloaded inventory.Inventory
		Expect(yaml.Unmarshal(saved, &reloaded)).To(Succeed())
		Expect(reloaded.AddressLookup.Candidates[0].EvidenceRef).To(BeEmpty())
		Canonicalize(&reloaded)
		Expect(reloaded.Fingerprint).To(Equal(before.Fingerprint), "%#v", Diff(&before, &reloaded))
		Expect(Diff(&before, &reloaded)).To(BeEmpty())
		after, err := json.Marshal(before)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(original), "comparison must not mutate the source inventory")
		lookupAfterJSON, err := json.Marshal(before.AddressLookup)
		Expect(err).NotTo(HaveOccurred())
		Expect(lookupAfterJSON).To(Equal(originalLookup), "material projection must not strip provenance from its input")

		for _, change := range []struct {
			name string
			edit func(*inventory.AddressCandidate)
		}{
			{name: "name", edit: func(candidate *inventory.AddressCandidate) { candidate.Name = "dpu-b" }},
			{name: "role", edit: func(candidate *inventory.AddressCandidate) { candidate.Role = "hosts" }},
			{name: "address", edit: func(candidate *inventory.AddressCandidate) { candidate.Address = "192.0.2.45" }},
			{name: "MAC", edit: func(candidate *inventory.AddressCandidate) { candidate.MAC = "02:00:00:00:00:02" }},
			{name: "source", edit: func(candidate *inventory.AddressCandidate) { candidate.Source = "arp-table" }},
		} {
			By(change.name)
			current := reloaded
			current.AddressLookup = &inventory.AddressLookup{Candidates: slices.Clone(reloaded.AddressLookup.Candidates), Observations: maps.Clone(reloaded.AddressLookup.Observations)}
			Expect(Diff(&reloaded, &current)).To(BeEmpty(), "the candidate edit must be the only material change")
			change.edit(&current.AddressLookup.Candidates[0])
			Canonicalize(&current)
			Expect(current.Fingerprint).NotTo(Equal(reloaded.Fingerprint))
			Expect(Diff(&reloaded, &current)).NotTo(BeEmpty())
		}
		for _, change := range []struct {
			name string
			edit func(*inventory.Observation)
		}{
			{name: "state", edit: func(observation *inventory.Observation) { observation.State = "unavailable" }},
			{name: "reason", edit: func(observation *inventory.Observation) { observation.ReasonCode = "ADDRESS_SOURCE_UNREADABLE" }},
		} {
			By(change.name)
			current := reloaded
			current.AddressLookup = &inventory.AddressLookup{Candidates: slices.Clone(reloaded.AddressLookup.Candidates), Observations: maps.Clone(reloaded.AddressLookup.Observations)}
			observation := current.AddressLookup.Observations["source.0"]
			change.edit(&observation)
			current.AddressLookup.Observations["source.0"] = observation
			Canonicalize(&current)
			Expect(current.Fingerprint).NotTo(Equal(reloaded.Fingerprint))
			Expect(Diff(&reloaded, &current)).NotTo(BeEmpty(), "source state and reason remain material")
		}
		current := reloaded
		current.AddressLookup = &inventory.AddressLookup{Candidates: slices.Clone(reloaded.AddressLookup.Candidates), Observations: maps.Clone(reloaded.AddressLookup.Observations)}
		candidate := current.AddressLookup.Candidates[0]
		candidate.EvidenceRef = "different-transient-ref"
		current.AddressLookup.Candidates[0] = candidate
		source := current.AddressLookup.Observations["source.0"]
		source.Values = []string{"source-bytes-digest-new"}
		current.AddressLookup.Observations["source.0"] = source
		device := current.AddressLookup.Observations["device.dpu-a"]
		device.Values = []string{"192.0.2.44"}
		current.AddressLookup.Observations["device.dpu-a"] = device
		Canonicalize(&current)
		Expect(current.Fingerprint).To(Equal(reloaded.Fingerprint))
		Expect(Diff(&reloaded, &current)).To(BeEmpty())

		current = reloaded
		current.AddressLookup = &inventory.AddressLookup{Candidates: []inventory.AddressCandidate{}, Observations: maps.Clone(reloaded.AddressLookup.Observations)}
		device = current.AddressLookup.Observations["device.dpu-a"]
		device.State, device.Values, device.ReasonCode = "unknown", nil, "ADDRESS_LEASE_EXPIRED"
		current.AddressLookup.Observations["device.dpu-a"] = device
		Canonicalize(&current)
		Expect(current.Fingerprint).NotTo(Equal(reloaded.Fingerprint), "expired candidate status remains material")
		Expect(Diff(&reloaded, &current)).NotTo(BeEmpty())

		request, err := addresslookup.Validate(addresslookup.Config{
			Sources: []addresslookup.Source{{File: "leases", Format: "dnsmasq-leases"}},
			Devices: []addresslookup.Selector{{Name: "dpu-a", Role: "arm", MAC: "02:00:00:00:00:01"}},
		})
		Expect(err).NotTo(HaveOccurred())
		first, firstEvidence := addresslookup.Collect(request, time.Unix(100, 0), func(string, int64) ([]byte, error) {
			return []byte("200 02:00:00:00:00:01 192.0.2.44 host-one *\n"), nil
		}, nil)
		second, secondEvidence := addresslookup.Collect(request, time.Unix(100, 0), func(string, int64) ([]byte, error) {
			return []byte("200 02:00:00:00:00:01 192.0.2.44 host-two *\n"), nil
		}, nil)
		Expect(firstEvidence).NotTo(BeEmpty())
		Expect(secondEvidence).NotTo(BeEmpty())
		Expect(first.Candidates[0].EvidenceRef).NotTo(Equal(second.Candidates[0].EvidenceRef))
		beforeLookup, afterLookup := estate(), estate()
		beforeLookup.AddressLookup, afterLookup.AddressLookup = first, second
		Canonicalize(&beforeLookup)
		Canonicalize(&afterLookup)
		Expect(afterLookup.Fingerprint).To(Equal(beforeLookup.Fingerprint))
		Expect(Diff(&beforeLookup, &afterLookup)).To(BeEmpty())
	})
})
