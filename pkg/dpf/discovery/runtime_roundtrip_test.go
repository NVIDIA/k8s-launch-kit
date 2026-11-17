// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("runtime material round trips", func() {
	It("treats empty runtime collections consistently after v3 YAML persistence", func() {
		fresh := runtimeMaterialInventory(false)
		explicitEmpty := runtimeMaterialInventory(true)
		beforeRuntime, err := json.Marshal(fresh.DPUs[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		beforeDevice, err := json.Marshal(fresh.DPUs[0])
		Expect(err).NotTo(HaveOccurred())
		beforeHostRuntime, err := json.Marshal(fresh.Hosts[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		resetTypesNil := fresh.DPUs[0].AdvertisedResetTypes == nil
		functionsNil := fresh.DPUs[0].Adapters[0].PCIFunctions == nil

		Canonicalize(&fresh)
		Canonicalize(&explicitEmpty)
		Expect(explicitEmpty.Fingerprint).To(Equal(fresh.Fingerprint))
		Expect(Diff(&fresh, &explicitEmpty)).To(BeEmpty())
		Expect(fresh.DPUs[0].Runtime.Network.Resolver.Domains).To(BeNil())

		saved, err := yaml.Marshal(fresh)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(saved)).To(ContainSubstring("links: []"))
		Expect(string(saved)).To(ContainSubstring("search: []"))
		Expect(string(saved)).To(ContainSubstring("perLink: {}"))
		Expect(string(saved)).To(ContainSubstring("domains: {}"))
		Expect(string(saved)).To(ContainSubstring("neighbors: []"))
		Expect(string(saved)).To(ContainSubstring("lldp: []"))

		var reloaded inventory.Inventory
		Expect(yaml.Unmarshal(saved, &reloaded)).To(Succeed())
		Expect(reloaded.DPUs).To(HaveLen(1))
		Expect(reloaded.Hosts).To(HaveLen(1))
		Expect(reloaded.DPUs[0].Runtime.Network.Links).NotTo(BeNil(), "the wire representation contains an explicit empty sequence")
		Expect(reloaded.DPUs[0].Runtime.Network.Resolver.Search).NotTo(BeNil())
		Expect(reloaded.DPUs[0].Runtime.Network.Resolver.PerLink).NotTo(BeNil(), "the wire representation contains an explicit empty map")
		Expect(reloaded.DPUs[0].Runtime.Network.Neighbors).NotTo(BeNil())
		Expect(reloaded.DPUs[0].Runtime.Network.LLDP).NotTo(BeNil())
		Expect(reloaded.DPUs[0].Runtime.Network.Resolver.Domains).NotTo(BeNil())
		Expect(reloaded.Hosts[0].Runtime.Network.Links).NotTo(BeNil())

		Canonicalize(&reloaded)
		Expect(reloaded.Fingerprint).To(Equal(fresh.Fingerprint))
		Expect(Diff(&fresh, &reloaded)).To(BeEmpty(), "%#v", Diff(&fresh, &reloaded))
		Expect(reloaded.DPUs[0].Runtime.Observations["resolver-dns"].ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"), "failed optional-parser diagnostics remain material")
		Expect(reloaded.Hosts[0].Runtime.Observations["resolver-dns"].ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))

		afterRuntime, err := json.Marshal(fresh.DPUs[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		Expect(afterRuntime).To(Equal(beforeRuntime), "material canonicalization must not mutate runtime observations")
		afterDevice, err := json.Marshal(fresh.DPUs[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(afterDevice).To(Equal(beforeDevice), "material canonicalization must not strip source facts")
		afterHostRuntime, err := json.Marshal(fresh.Hosts[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		Expect(afterHostRuntime).To(Equal(beforeHostRuntime), "material canonicalization must not mutate host observations")
		Expect((fresh.DPUs[0].AdvertisedResetTypes == nil)).To(Equal(resetTypesNil))
		Expect((fresh.DPUs[0].Adapters[0].PCIFunctions == nil)).To(Equal(functionsNil))

		changed := reloaded
		changed.DPUs = append([]inventory.Device(nil), reloaded.DPUs...)
		runtime := *reloaded.DPUs[0].Runtime
		runtime.Interfaces = slices.Clone(runtime.Interfaces)
		for i := range runtime.Interfaces {
			runtime.Interfaces[i].Addresses = slices.Clone(runtime.Interfaces[i].Addresses)
		}
		changed.DPUs[0].Runtime = &runtime
		changed.DPUs[0].Runtime.Interfaces[0].Addresses = []string{"192.0.2.45"}
		Canonicalize(&changed)
		Expect(changed.Fingerprint).NotTo(Equal(reloaded.Fingerprint), "a real host address change remains material")
		Expect(Diff(&reloaded, &changed)).NotTo(BeEmpty())
	})

	It("normalizes omitted top-level runtime slices without losing source immutability", func() {
		missing := runtimeMaterialInventory(false)
		empty := runtimeMaterialInventory(false)
		missing.DPUs[0].Runtime.Interfaces = nil
		empty.DPUs[0].Runtime.Interfaces = []inventory.Interface{}
		before, err := json.Marshal(missing.DPUs[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		Canonicalize(&missing)
		Canonicalize(&empty)
		Expect(empty.Fingerprint).To(Equal(missing.Fingerprint))
		Expect(Diff(&missing, &empty)).To(BeEmpty())
		after, err := json.Marshal(missing.DPUs[0].Runtime)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("compares empty collections consistently after v3 JSON persistence", func() {
		fresh := runtimeMaterialInventory(false)
		Canonicalize(&fresh)
		empty := runtimeMaterialInventory(true)
		saved, err := json.Marshal(empty)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(saved)).To(ContainSubstring(`"links":[]`))
		Expect(string(saved)).To(ContainSubstring(`"search":[]`))
		Expect(string(saved)).To(ContainSubstring(`"perLink":{}`))
		Expect(string(saved)).To(ContainSubstring(`"domains":{}`))
		Expect(string(saved)).To(ContainSubstring(`"neighbors":[]`))
		var reloaded inventory.Inventory
		Expect(json.Unmarshal(saved, &reloaded)).To(Succeed())
		Canonicalize(&reloaded)
		Expect(reloaded.Fingerprint).To(Equal(fresh.Fingerprint))
		Expect(Diff(&fresh, &reloaded)).To(BeEmpty())
		Expect(reloaded.DPUs[0].Runtime.Network.Resolver.Domains).NotTo(BeNil())
		Expect(reloaded.Hosts[0].Runtime.Observations["resolver-dns"].ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
	})

	It("preserves absence of whole Network and Hardware observations", func() {
		without := runtimeMaterialInventory(false)
		without.DPUs[0].Runtime.Network = nil
		without.DPUs[0].Runtime.Hardware = nil
		withEmpty := runtimeMaterialInventory(false)
		withEmpty.DPUs[0].Runtime.Network = &inventory.NetworkObservation{}
		withEmpty.DPUs[0].Runtime.Hardware = &inventory.HardwareObservation{}
		Canonicalize(&without)
		Canonicalize(&withEmpty)
		Expect(withEmpty.Fingerprint).NotTo(Equal(without.Fingerprint))
		Expect(Diff(&without, &withEmpty)).NotTo(BeEmpty())
	})

	It("preserves resolver map keys and real resolver values", func() {
		without := runtimeMaterialInventory(false)
		withFact := runtimeMaterialInventory(false)
		withFact.DPUs[0].Runtime.Network.Resolver.PerLink = map[string][]string{"evidence": {"192.0.2.53"}}
		before, err := json.Marshal(withFact.DPUs[0].Runtime.Network.Resolver.PerLink)
		Expect(err).NotTo(HaveOccurred())
		Canonicalize(&without)
		Canonicalize(&withFact)
		Expect(withFact.Fingerprint).NotTo(Equal(without.Fingerprint))
		Expect(Diff(&without, &withFact)).NotTo(BeEmpty())
		after, err := json.Marshal(withFact.DPUs[0].Runtime.Network.Resolver.PerLink)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before), "material normalization must retain arbitrary map keys and values")
	})
})

func runtimeMaterialInventory(empty bool) inventory.Inventory {
	inv := estate()
	inv.FormatVersion = inventory.FormatVersion
	inv.DPUs = inv.DPUs[:1]
	dpu := &inv.DPUs[0]
	dpu.AdvertisedResetTypes = nil
	functions := []inventory.PCIFunction{{Address: "0000:01:00.0", Driver: "mlx5_core"}}
	adapterFunctions := []inventory.PCIFunction(nil)
	if empty {
		dpu.AdvertisedResetTypes = []string{}
		adapterFunctions = []inventory.PCIFunction{}
		functions[0].Netdevs = []string{}
	}
	dpu.Adapters = []inventory.Adapter{{Key: "adapter-a", SerialNumber: "CARD-A", SerialNumberRaw: "CARD-A", Model: "BlueField", IdentityState: "known", Selection: "selected", PCIFunctions: adapterFunctions}, {Key: "adapter-b", SerialNumber: "CARD-B", SerialNumberRaw: "CARD-B", Model: "BlueField", IdentityState: "known", Selection: "selected", PCIFunctions: functions}, {Key: "adapter-c", SerialNumber: "CARD-C", SerialNumberRaw: "CARD-C", Model: "BlueField", IdentityState: "known", Selection: "selected", PCIFunctions: []inventory.PCIFunction{{Address: "0000:02:00.0", Netdevs: []string{"ens6f0", "ens5f0"}}}}}
	dpuRuntime := runtimeMaterialObservation(empty)
	dpu.Runtime = &dpuRuntime
	hostRuntime := runtimeMaterialObservation(empty)
	inv.Hosts[0].Runtime = &hostRuntime
	return inv
}

func runtimeMaterialObservation(empty bool) inventory.RuntimeObservation {
	interfaceAddresses := []string(nil)
	interfaces := []inventory.Interface{{Name: "data1", Index: 5, MTU: 9000, Addresses: interfaceAddresses}, {Name: "data0", Index: 4, MTU: 9000, Addresses: []string{"192.0.2.44"}}}
	var routes []inventory.Route
	var dns []string
	network := &inventory.NetworkObservation{
		Resolver: inventory.ResolverObservation{Owner: "systemd-resolved"},
	}
	if empty {
		interfaces[0].Addresses = []string{}
		routes = []inventory.Route{}
		dns = []string{}
		network.Links = []inventory.NetworkLink{}
		network.Resolver.Search = []string{}
		network.Resolver.PerLink = map[string][]string{}
		network.Resolver.Domains = map[string][]string{}
		network.Resolver.DNSMasqServers = []string{}
		network.Neighbors = []inventory.Neighbor{}
		network.LLDP = []inventory.LLDPNeighbor{}
	}
	return inventory.RuntimeObservation{
		Interfaces: interfaces,
		Routes:     routes,
		DNS:        dns,
		Observations: map[string]inventory.Observation{
			"interfaces":       {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "ip"},
			"routes":           {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "ip"},
			"dns":              {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "cat"},
			"network-links":    {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "ip"},
			"neighbors":        {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "ip"},
			"resolver-dns":     {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "resolvectl"},
			"resolver-domains": {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "resolvectl"},
			"dnsmasq":          {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "sh"},
			"lldp":             {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "lldpctl"},
		},
		Network: network,
	}
}
