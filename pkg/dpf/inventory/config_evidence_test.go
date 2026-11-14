// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = ginkgo.Describe("discovered configuration diagnostics", func() {
	ginkgo.It("omits typed provenance fields while preserving arbitrary data map keys", func() {
		device := outputDeviceFixture()
		device.Runtime = &RuntimeObservation{
			Network: &NetworkObservation{Resolver: ResolverObservation{
				PerLink: map[string][]string{"evidence": {"192.0.2.10"}},
				Domains: map[string][]string{"evidenceRefs": {"example.test"}},
			}},
		}
		in := Inventory{
			FormatVersion: FormatVersion, ScopeDigest: "scope", Fingerprint: "facts",
			Management:    &Management{APIEndpoint: "https://api.example", SystemNamespaceUID: "cluster", EvidenceRefs: []string{"management"}},
			DPUs:          []Device{device},
			AddressLookup: &AddressLookup{Candidates: []AddressCandidate{{Address: "192.0.2.2", EvidenceRef: "lookup"}}},
			Probes:        []ProbeResult{{NodeUID: "node", Destination: "api", NetworkMode: "pod", DNS: "passed", EvidenceRef: "probe"}},
			Evidence:      map[string]Evidence{"management": {Kind: "kubernetes"}, "host": {Kind: "ssh"}, "probe": {Kind: "probe"}},
		}
		before, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		data, err := yaml.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var encoded map[string]any
		Expect(yaml.Unmarshal(data, &encoded)).To(Succeed())
		Expect(encoded).NotTo(HaveKey("evidence"))
		Expect(mapAt(encoded, "management")).NotTo(HaveKey("evidenceRefs"))
		addressLookup := mapAt(encoded, "addressLookup")
		Expect(addressLookup["candidates"].([]any)[0].(map[string]any)).NotTo(HaveKey("evidenceRef"))
		Expect(encoded["probes"].([]any)[0].(map[string]any)).NotTo(HaveKey("evidenceRef"))

		resolver := encoded["dpus"].([]any)[0].(map[string]any)
		resolver = mapAt(resolver, "runtime", "network", "resolver")
		Expect(mapAt(resolver, "perLink")).To(HaveKeyWithValue("evidence", []any{"192.0.2.10"}))
		Expect(mapAt(resolver, "domains")).To(HaveKeyWithValue("evidenceRefs", []any{"example.test"}))
		var restored Inventory
		Expect(yaml.Unmarshal(data, &restored)).To(Succeed())
		Expect(restored.ScopeDigest).To(Equal(in.ScopeDigest))
		Expect(restored.Fingerprint).To(Equal(in.Fingerprint))
		Expect(restored.Management.APIEndpoint).To(Equal(in.Management.APIEndpoint))
		Expect(restored.DPUs[0].SerialNumber).To(Equal(in.DPUs[0].SerialNumber))
		Expect(restored.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
		Expect(restored.Probes[0].DNS).To(Equal("passed"))
		Expect(restored.AddressLookup.Candidates[0].Address).To(Equal("192.0.2.2"))
		Expect(restored.Evidence).To(BeEmpty())
		Expect(restored.DPUs[0].Runtime.Network.Resolver.PerLink).To(HaveKeyWithValue("evidence", []string{"192.0.2.10"}))
		Expect(restored.DPUs[0].Runtime.Network.Resolver.Domains).To(HaveKeyWithValue("evidenceRefs", []string{"example.test"}))
		after, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})
	ginkgo.It("reads older evidence-bearing configs but drops bookkeeping on save", func() {
		data := []byte("formatVersion: v1alpha2\nmanagement:\n  apiEndpoint: https://api.example\n  evidenceRefs: [old]\nevidence:\n  old:\n    kind: kubernetes\n")
		var in Inventory
		Expect(yaml.Unmarshal(data, &in)).To(Succeed())
		Expect(in.Evidence).To(HaveKey("old"))
		in.Management.Observations = map[string]Observation{"evidence": {State: "observed", Values: []string{"kept"}}}
		in.DPUs = []Device{{
			RecordKey: "legacy-device", EvidenceRefs: []string{"device-evidence"},
			Firmware: []Firmware{{Component: "os", EvidenceRefs: []string{"firmware-evidence"}}},
		}}
		saved, err := yaml.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var encoded map[string]any
		Expect(yaml.Unmarshal(saved, &encoded)).To(Succeed())
		Expect(encoded).NotTo(HaveKey("evidence"))
		management := mapAt(encoded, "management")
		Expect(management).NotTo(HaveKey("evidenceRefs"))
		Expect(mapAt(management, "observations")).To(HaveKey("evidence"))
		legacyDPU := encoded["dpus"].([]any)[0].(map[string]any)
		Expect(legacyDPU).NotTo(HaveKey("evidenceRefs"))
		Expect(legacyDPU["firmware"].([]any)[0].(map[string]any)).NotTo(HaveKey("evidenceRefs"))
		Expect(string(saved)).NotTo(ContainSubstring("device-evidence"))
		Expect(string(saved)).NotTo(ContainSubstring("firmware-evidence"))
		Expect(string(saved)).To(ContainSubstring("https://api.example"))
	})
})

func mapAt(root map[string]any, path ...string) map[string]any {
	for _, key := range path {
		value, ok := root[key].(map[string]any)
		Expect(ok).To(BeTrue(), "expected mapping at %q", key)
		root = value
	}
	return root
}
