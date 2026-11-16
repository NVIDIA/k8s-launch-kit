// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = ginkgo.Describe("legacy inventory input", func() {
	ginkgo.It("discards retired metadata while preserving decoded facts", func() {
		for _, version := range []string{"v1alpha1", "v1alpha2"} {
			for _, encoding := range []string{"json", "yaml"} {
				var baseline Inventory
				base := legacyInventoryDocument(version, encoding, 0)
				if encoding == "json" {
					Expect(json.Unmarshal([]byte(base), &baseline)).To(Succeed())
				} else {
					Expect(yaml.Unmarshal([]byte(base), &baseline)).To(Succeed())
				}
				for _, retired := range []int{1, 2, 3} {
					data := []byte(legacyInventoryDocument(version, encoding, retired))
					var got Inventory
					if encoding == "json" {
						Expect(json.Unmarshal(data, &got)).To(Succeed(), "%s %s", version, encoding)
					} else {
						Expect(yaml.Unmarshal(data, &got)).To(Succeed(), "%s %s", version, encoding)
					}
					Expect(got).To(Equal(baseline), "%s %s metadata mask %d", version, encoding, retired)
					for _, device := range append(append([]Device{}, got.Hosts...), got.DPUs...) {
						Expect(device.IdentityKey).To(BeEmpty())
						Expect(device.SerialNumber).To(BeEmpty())
						Expect(device.Authentication.BMCAdministrator).To(BeEmpty())
					}
					var output []byte
					var err error
					if encoding == "json" {
						output, err = json.Marshal(got)
					} else {
						output, err = yaml.Marshal(got)
					}
					Expect(err).NotTo(HaveOccurred())
					Expect(string(output)).NotTo(ContainSubstring("qualification"))
					Expect(string(output)).NotTo(ContainSubstring("inventoryCompleteness"))
				}
			}
		}
	})

	ginkgo.It("transfers every top-level legacy inventory field", func() {
		for _, version := range []string{"v1alpha1", "v1alpha2"} {
			want := fullLegacyInventory(version)
			if version == "v1alpha2" {
				Expect(NormalizeInventory(&want)).To(Succeed())
			}
			for _, encoding := range []string{"json", "yaml"} {
				var data []byte
				var err error
				if encoding == "json" {
					data, err = json.Marshal(inventoryWire(fullLegacyInventory(version)))
				} else {
					data, err = yaml.Marshal(inventoryWire(fullLegacyInventory(version)))
				}
				Expect(err).NotTo(HaveOccurred())
				var got Inventory
				if encoding == "json" {
					err = json.Unmarshal(data, &got)
				} else {
					err = yaml.Unmarshal(data, &got)
				}
				Expect(err).NotTo(HaveOccurred(), "%s %s", version, encoding)
				Expect(got).To(Equal(want), "%s %s", version, encoding)
			}
		}
	})

	ginkgo.It("keeps strict validation for unknown fields, malformed retired values, and modern versions", func() {
		for _, input := range []struct {
			name string
			json string
			yaml string
		}{
			{name: "unknown legacy nested field", json: `{"formatVersion":"v1alpha2","dpus":[{"id":"dpu-one","unexpected":true}]}`, yaml: "formatVersion: v1alpha2\ndpus: [{id: dpu-one, unexpected: true}]\n"},
			{name: "qualification object", json: `{"formatVersion":"v1alpha2","hosts":[{"qualification":{}}]}`, yaml: "formatVersion: v1alpha2\nhosts: [{qualification: {}}]\n"},
			{name: "completeness sequence", json: `{"formatVersion":"v1alpha2","dpus":[{"inventoryCompleteness":[]}]}`, yaml: "formatVersion: v1alpha2\ndpus: [{inventoryCompleteness: [old]}]\n"},
			{name: "modern qualification", json: fmt.Sprintf(`{"formatVersion":%q,"dpus":[{"qualification":"old"}]}`, FormatVersion), yaml: fmt.Sprintf("formatVersion: %s\ndpus: [{qualification: old}]\n", FormatVersion)},
			{name: "modern completeness", json: fmt.Sprintf(`{"formatVersion":%q,"hosts":[{"inventoryCompleteness":"old"}]}`, FormatVersion), yaml: fmt.Sprintf("formatVersion: %s\nhosts: [{inventoryCompleteness: old}]\n", FormatVersion)},
		} {
			var fromJSON, fromYAML Inventory
			Expect(json.Unmarshal([]byte(input.json), &fromJSON)).To(HaveOccurred(), input.name)
			Expect(yaml.Unmarshal([]byte(input.yaml), &fromYAML)).To(HaveOccurred(), input.name)
		}

		for _, decode := range []func(*Inventory) error{
			func(result *Inventory) error {
				return json.Unmarshal([]byte(`{"formatVersion":"v1alpha2","dpus":[{"unexpected":true}]}`), result)
			},
			func(result *Inventory) error {
				return yaml.Unmarshal([]byte("formatVersion: v1alpha2\ndpus: [{unexpected: true}]\n"), result)
			},
		} {
			result := Inventory{FormatVersion: "v1alpha1", Hosts: []Device{{ID: "unchanged"}}}
			before := result
			Expect(decode(&result)).To(HaveOccurred())
			Expect(result).To(Equal(before))
		}
	})

	ginkgo.It("preserves nil and empty legacy device slices", func() {
		for _, devices := range [][]Device{nil, {}} {
			input := Inventory{FormatVersion: "v1alpha1", Hosts: devices, DPUs: devices}
			encoded, err := json.Marshal(input)
			Expect(err).NotTo(HaveOccurred())
			var got Inventory
			Expect(json.Unmarshal(encoded, &got)).To(Succeed())
			Expect(got.Hosts == nil).To(Equal(input.Hosts == nil))
			Expect(got.DPUs == nil).To(Equal(input.DPUs == nil))
			Expect(got.Hosts).To(Equal(input.Hosts))
			Expect(got.DPUs).To(Equal(input.DPUs))
		}
	})
})

func legacyInventoryDocument(version, encoding string, metadata int) string {
	if encoding == "json" {
		device := func(id string) string {
			fields := ""
			if metadata&1 != 0 {
				fields += `,"qualification":"legacy qualification"`
			}
			if metadata&2 != 0 {
				fields += `,"inventoryCompleteness":"legacy completeness"`
			}
			return fmt.Sprintf(`{"id":%q%s}`, id, fields)
		}
		return fmt.Sprintf(`{"formatVersion":%q,"hosts":[%s],"dpus":[%s]}`, version, device("host-one"), device("dpu-one"))
	}
	device := func(id string) string {
		out := "  - id: " + id + "\n"
		if metadata&1 != 0 {
			out += "    qualification: legacy-qualification\n"
		}
		if metadata&2 != 0 {
			out += "    inventoryCompleteness: legacy-completeness\n"
		}
		return out
	}
	return fmt.Sprintf("formatVersion: %s\nhosts:\n%sdpus:\n%s", version, device("host-one"), device("dpu-one"))
}

func fullLegacyInventory(version string) Inventory {
	return Inventory{
		Software:       &releases.Release{Version: "release-test", Components: []releases.Component{}, Hardware: []releases.HardwareBOM{}, Sources: []releases.Source{}, Limitations: []string{}},
		AddressLookup:  &AddressLookup{Candidates: []AddressCandidate{{Name: "candidate", Role: "dpu", Address: "192.0.2.10", Source: "test", EvidenceRef: "address-evidence"}}, Observations: map[string]Observation{"lookup": {State: "observed", ReasonCode: "LOOKUP_OK"}}},
		FormatVersion:  version,
		Collection:     Collection{RunID: "run-test", StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), FinishedAt: time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC), Mode: "inventory", Collector: "test"},
		RequestedScope: []ScopeEndpoint{{Role: "host", Endpoint: "ssh://host.example", TLSTrustDigest: "tls-digest", SSHTrustDigest: "ssh-digest", SSHSudo: true, TLSInsecureSkipVerify: true}},
		ScopeDigest:    "scope-digest", Fingerprint: "fingerprint",
		Management:   &Management{APIEndpoint: "https://api.example", SystemNamespaceUID: "system-uid", TrustDigest: "trust-digest", KubernetesVersion: "v1.2.3", Namespace: "system", Nodes: []Node{{Name: "node-one", UID: "node-uid", ControlPlane: true, Ready: true, Architecture: "amd64", OS: "linux", Addresses: []string{"192.0.2.20"}}}, Observations: map[string]Observation{"api": {State: "observed", ReasonCode: "API_OK"}}, EvidenceRefs: []string{"management-evidence"}},
		DPUs:         []Device{{ID: "dpu-one", Model: "dpu-model"}},
		Hosts:        []Device{{ID: "host-one", Model: "host-model"}},
		Associations: []Association{{DPUID: "dpu-one", HostID: "host-one", HostAdapterKey: "adapter-key", SerialNumber: "serial"}},
		Probes:       []ProbeResult{{NodeUID: "node-uid", NetworkMode: "host", Destination: "192.0.2.10", DNS: "ok", Addresses: []ProbeAddress{{Address: "192.0.2.10", TCP: "ok", TLS: "ok", HTTP: "ok", HTTPStatus: 200}}, EvidenceRef: "probe-evidence"}},
		Evidence:     map[string]Evidence{"probe-evidence": {Kind: "probe", ObservedAt: time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), Vantage: "node", Authenticated: true}},
	}
}
