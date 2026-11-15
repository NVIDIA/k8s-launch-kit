// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("persisted management fact comparison", func() {
	It("keeps management facts stable across supported codecs without mutating inputs", func() {
		for _, version := range []string{"v1alpha1", "v1alpha2", inventory.FormatVersion} {
			for _, codec := range []struct {
				name   string
				encode func(any) ([]byte, error)
				decode func([]byte, any) error
			}{{name: "YAML", encode: yaml.Marshal, decode: yaml.Unmarshal}, {name: "JSON", encode: json.Marshal, decode: json.Unmarshal}} {
				for _, node := range []inventory.Node{
					{Name: "cp-a", UID: "node-a", Addresses: nil, Interfaces: nil, Routes: nil},
					{Name: "cp-a", UID: "node-a", Addresses: []string{}, Interfaces: []inventory.Interface{}, Routes: []inventory.Route{}},
					{Name: "cp-a", UID: "node-a", Addresses: []string{"192.0.2.10"}, Interfaces: []inventory.Interface{{Name: "data0", Index: 2, MTU: 9000, Addresses: nil}}},
					{Name: "cp-a", UID: "node-a", Addresses: []string{"192.0.2.10"}, Interfaces: []inventory.Interface{{Name: "data0", Index: 2, MTU: 9000, Addresses: []string{}}}},
				} {
					By(fmt.Sprintf("%s %s addresses=%#v interfaces=%#v", version, codec.name, node.Addresses, node.Interfaces))
					source := inventory.Inventory{FormatVersion: version, Management: &inventory.Management{APIEndpoint: "https://api.example.test", SystemNamespaceUID: "cluster-a", Nodes: []inventory.Node{node}}}
					originalNode, err := json.Marshal(source.Management.Nodes)
					Expect(err).NotTo(HaveOccurred())
					Canonicalize(&source)
					encoded, err := codec.encode(source)
					Expect(err).NotTo(HaveOccurred())
					var restored inventory.Inventory
					Expect(codec.decode(encoded, &restored)).To(Succeed())
					Canonicalize(&restored)
					Expect(restored.Fingerprint).To(Equal(source.Fingerprint))
					Expect(Diff(&source, &restored)).To(BeEmpty())
					afterNode, err := json.Marshal(source.Management.Nodes)
					Expect(err).NotTo(HaveOccurred())
					Expect(afterNode).To(Equal(originalNode), "material normalization must not mutate source nodes")
					restored.Management.Nodes[0].Addresses = []string{"198.51.100.10"}
					Canonicalize(&restored)
					Expect(restored.Fingerprint).NotTo(Equal(source.Fingerprint))
					Expect(Diff(&source, &restored)).NotTo(BeEmpty(), "a real node address change must remain visible")
				}
			}
		}
	})
})

var _ = Describe("persisted host permanent MAC facts", func() {
	It("preserves the concrete permanent address and detects an actual hardware identity change", func() {
		for _, codec := range []struct {
			name   string
			encode func(any) ([]byte, error)
			decode func([]byte, any) error
		}{{name: "YAML", encode: yaml.Marshal, decode: yaml.Unmarshal}, {name: "JSON", encode: json.Marshal, decode: json.Unmarshal}} {
			By(codec.name)
			source := inventory.Inventory{FormatVersion: inventory.FormatVersion, Hosts: []inventory.Device{{
				ID: "host-one", RecordKey: "host-one", ObservationState: "observed", Qualification: "unqualified",
				SSHEndpoint: "ssh://192.0.2.11:22", Authentication: inventory.Authentication{Method: "ssh"},
				Runtime: &inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{
					Name: "data0", PCIAddress: "0000:21:00.0", MAC: "02:00:00:00:00:99", PermanentMAC: "02:00:00:00:00:01",
				}}}},
			}}}
			Canonicalize(&source)
			encoded, err := codec.encode(source)
			Expect(err).NotTo(HaveOccurred())
			var restored inventory.Inventory
			Expect(codec.decode(encoded, &restored)).To(Succeed())
			Expect(restored.Hosts[0].Runtime.Network.Links[0].PermanentMAC).To(Equal("02:00:00:00:00:01"))
			Expect(restored.Hosts[0].Runtime.Network.Links[0].MAC).To(Equal("02:00:00:00:00:99"))
			Canonicalize(&restored)
			Expect(Diff(&source, &restored)).To(BeEmpty())
			Expect(restored.Fingerprint).To(Equal(source.Fingerprint))
			restored.Hosts[0].Runtime.Network.Links[0].PermanentMAC = "02:00:00:00:00:02"
			Canonicalize(&restored)
			Expect(restored.Fingerprint).NotTo(Equal(source.Fingerprint))
			Expect(Diff(&source, &restored)).NotTo(BeEmpty())
		}
	})
})
