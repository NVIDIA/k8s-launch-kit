// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN selected routing-policy checks", func() {
	var inv inventory.Inventory
	var configText string
	BeforeEach(func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		allocatedASNs, err := profile.AllocateASNs([]string{"DPU-A"})
		Expect(err).NotTo(HaveOccurred())
		inv = inventory.Inventory{HBN: &inventory.HBNInventory{
			Profile: profile,
			ASNs:    []inventory.ASNAssignment{{DPUSerial: "DPU-A", ASN: allocatedASNs["DPU-A"]}},
			Switches: []inventory.FabricSwitch{{
				Name:  "leaf-a",
				Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "DPU-A", DPUInterface: "p0"}},
				Snapshot: fabric.Snapshot{
					Configured: fabric.ConfiguredFacts{VRFs: []fabric.VRFConfig{{Name: profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}},
				},
			}},
		}}
		configText = "frr defaults datacenter\n" +
			"router bgp 65000 vrf " + profile.FabricVRF + "\n" +
			" neighbor fabric peer-group\n" +
			" neighbor fabric remote-as external\n" +
			" neighbor swp1 interface peer-group fabric\n" +
			" address-family ipv4 unicast\n" +
			"  neighbor fabric activate\n" +
			" exit-address-family\n"
	})
	check := func() inventory.Check {
		checks := hbnRoutingPolicyChecks(&inv)
		Expect(checks).To(HaveLen(1))
		return checks[0]
	}
	It("passes the selected unfiltered peer under a known permissive eBGP mode", func() {
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(configText)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}

		got := check()
		Expect(got.State).To(Equal("passed"), got.ReasonCode, got.Message)
		Expect(got.ReasonCode).To(Equal("ROUTING_POLICY_UNFILTERED"))
		Expect(got.Message).To(ContainSubstring("runtime routes are not verified"))
	})
	It("validates the direct unnumbered baseline and rejects a mutually wrong remote ASN", func() {
		assigned := inv.HBN.ASNs[0].ASN
		for _, remote := range []uint32{assigned, assigned + 1} {
			remoteText := strconv.FormatUint(uint64(remote), 10)
			text := "frr defaults datacenter\nrouter bgp 65000 vrf " + inv.HBN.Profile.FabricVRF + "\n" +
				" neighbor swp1 interface remote-as " + remoteText + "\n" +
				" neighbor swp1 advertisement-interval 0\n" +
				" neighbor swp1 timers 3 9\n" +
				" neighbor swp1 timers connect 10\n" +
				" address-family ipv4 unicast\n exit-address-family\n"
			inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(text)})
			inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: &[]fabric.BGPNeighborConfig{{Name: "swp1", EffectiveRemoteAS: &remoteText}}}}
			got := check()
			if remote == assigned {
				Expect(got.State).To(Equal("passed"), got.ReasonCode, got.Message)
			} else {
				Expect(got.State).To(Equal("failed"))
				Expect(got.ReasonCode).To(Equal("ROUTING_POLICY_REMOTE_AS_MISMATCH"))
			}
		}
	})
	It("keeps inherited and direct filters unresolved for only the selected IPv4 peer", func() {
		cases := []struct {
			name string
			line string
		}{
			{name: "peer group", line: "  neighbor fabric route-map filtered out\n"},
			{name: "neighbor", line: "  neighbor swp1 prefix-list filtered in\n"},
		}
		for _, tc := range cases {
			By(tc.name)
			inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(strings.Replace(configText, " exit-address-family", tc.line+" exit-address-family", 1))})
			inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
			got := check()
			Expect(got.State).To(Equal("unknown"))
			Expect(got.ReasonCode).To(Equal("ROUTING_POLICY_UNSUPPORTED"))
		}
	})
	It("does not let an unrelated VRF filter block the selected HBN peer", func() {
		configText += "router bgp 65001 vrf unrelated\n neighbor other peer-group\n neighbor other route-map other-map in\n neighbor swp99 password unrelated-secret\n"
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(configText)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().State).To(Equal("passed"))
	})
	It("keeps selected unsupported peer settings unknown without retaining secrets", func() {
		cases := []struct {
			setting string
			reason  string
		}{
			{setting: "password secret-value", reason: "ROUTING_POLICY_AUTHENTICATION_UNVERIFIED"},
			{setting: "local-as 65055", reason: "ROUTING_POLICY_LOCAL_AS_UNSUPPORTED"},
			{setting: "update-source loopback-secret", reason: "ROUTING_POLICY_TRANSPORT_UNSUPPORTED"},
		}
		for _, tc := range cases {
			text := strings.Replace(configText, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n neighbor swp1 "+tc.setting, 1)
			inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(text)})
			inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
			got := check()
			Expect(got.State).To(Equal("unknown"))
			Expect(got.ReasonCode).To(Equal(tc.reason))
			Expect(got.Message).NotTo(ContainSubstring("secret-value"))
			Expect(got.Message).NotTo(ContainSubstring("65055"))
			Expect(got.Message).NotTo(ContainSubstring("loopback-secret"))
		}
	})
	It("fails closed when eBGP requires a policy but the selected peer has none", func() {
		configText = strings.Replace(configText, " neighbor fabric peer-group", " bgp ebgp-requires-policy\n neighbor fabric peer-group", 1)
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(configText)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		got := check()
		Expect(got.State).To(Equal("failed"))
		Expect(got.ReasonCode).To(Equal("EBGP_REQUIRES_POLICY_ENABLED"))
	})
	It("fails for explicit BGP or peer shutdown and IPv4-unicast inactivity", func() {
		cases := []struct {
			name   string
			insert string
			reason string
			state  string
		}{
			{name: "VRF BGP shutdown", insert: " bgp shutdown\n", reason: "ROUTING_POLICY_BGP_SHUTDOWN", state: "failed"},
			{name: "peer group shutdown", insert: " neighbor fabric shutdown\n", reason: "ROUTING_POLICY_PEER_SHUTDOWN", state: "failed"},
			{name: "direct peer shutdown", insert: " neighbor swp1 shutdown\n", reason: "ROUTING_POLICY_PEER_SHUTDOWN", state: "failed"},
			{name: "global IPv4 default disabled", insert: " no bgp default ipv4-unicast\n", reason: "ROUTING_POLICY_PEER_INACTIVE", state: "failed"},
			{name: "group deactivation", insert: " no neighbor fabric activate\n", reason: "ROUTING_POLICY_PEER_INACTIVE", state: "failed"},
			{name: "direct deactivation", insert: " no neighbor swp1 activate\n", reason: "ROUTING_POLICY_PEER_INACTIVE", state: "failed"},
		}
		for _, tc := range cases {
			By(tc.name)
			text := configText
			switch tc.name {
			case "global IPv4 default disabled":
				text = strings.Replace(text, "  neighbor fabric activate\n", "", 1)
				text = strings.Replace(text, " neighbor fabric peer-group", " no bgp default ipv4-unicast\n neighbor fabric peer-group", 1)
			case "group deactivation":
				text = strings.Replace(text, "  neighbor fabric activate", "  neighbor fabric activate\n  no neighbor fabric activate", 1)
			case "direct deactivation":
				text = strings.Replace(text, "  neighbor fabric activate", "  neighbor fabric activate\n  no neighbor swp1 activate", 1)
			default:
				text = strings.Replace(text, " neighbor fabric peer-group", " neighbor fabric peer-group\n"+strings.TrimSuffix(tc.insert, "\n"), 1)
			}
			inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(text)})
			inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
			got := check()
			Expect(got.State).To(Equal(tc.state), tc.name)
			Expect(got.ReasonCode).To(Equal(tc.reason), tc.name)
		}
	})
	It("allows a direct activation and shutdown override of peer-group settings", func() {
		text := strings.Replace(configText, " neighbor fabric peer-group", " neighbor fabric peer-group\n neighbor fabric shutdown", 1)
		text = strings.Replace(text, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n no neighbor swp1 shutdown", 1)
		text = strings.Replace(text, "  neighbor fabric activate", "  no neighbor fabric activate\n  neighbor swp1 activate", 1)
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(text)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().State).To(Equal("passed"))
	})
	It("checks FRR local and effective remote ASNs against the resolved selection", func() {
		mismatchedLocal := strings.Replace(configText, "router bgp 65000", "router bgp 65001", 1)
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(mismatchedLocal)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().State).To(Equal("failed"))
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_LOCAL_AS_MISMATCH"))

		wrongRemote := strings.Replace(configText, "neighbor fabric remote-as external", "neighbor fabric remote-as 65102", 1)
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(wrongRemote)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().State).To(Equal("failed"))
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_REMOTE_AS_MISMATCH"))

		assigned, err := inv.HBN.Profile.AllocateASNs([]string{"DPU-A"})
		Expect(err).NotTo(HaveOccurred())
		directExpected := strings.Replace(configText, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n neighbor swp1 remote-as "+strconv.FormatUint(uint64(assigned["DPU-A"]), 10), 1)
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(directExpected)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().State).To(Equal("passed"))
	})
	It("recomputes saved DPU ASN assignments and requires NVUE remote-AS facts", func() {
		inv.HBN.ASNs[0].ASN++
		got := check()
		Expect(got.State).To(Equal("unknown"))
		Expect(got.ReasonCode).To(Equal("ROUTING_POLICY_ASN_ASSIGNMENT_UNKNOWN"))

		inv.HBN.ASNs[0].ASN--
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(configText)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		(*inv.HBN.Switches[0].Snapshot.Configured.VRFs[0].Neighbors)[0].EffectiveRemoteAS = nil
		got = check()
		Expect(got.State).To(Equal("unknown"))
		Expect(got.ReasonCode).To(Equal("ROUTING_POLICY_REMOTE_AS_UNKNOWN"))
	})
	It("keeps missing effective activation facts unknown", func() {
		inv.HBN.Switches[0].Snapshot = fabric.Snapshot{Configured: fabric.ConfiguredFacts{
			VRFs: []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}},
			FRRPolicy: &fabric.FRRPolicyConfig{Complete: true, VRFs: []fabric.FRRPolicyVRF{{
				Name: inv.HBN.Profile.FabricVRF, Complete: true,
				Neighbors:  []fabric.FRRPolicyNeighbor{{Name: "swp1", PeerGroup: stringPointer("fabric"), Interface: true}},
				PeerGroups: []fabric.FRRPolicyPeerGroup{{Name: "fabric"}},
			}}},
		}}
		Expect(check().State).To(Equal("unknown"))
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_BGP_STATE_UNKNOWN"))
		bgpEnabled := false
		localASN := uint32(65000)
		remoteAS := "external"
		inv.HBN.Switches[0].Snapshot.Configured.FRRPolicy.VRFs[0].BGPShutdown = &bgpEnabled
		inv.HBN.Switches[0].Snapshot.Configured.FRRPolicy.VRFs[0].LocalASN = &localASN
		inv.HBN.Switches[0].Snapshot.Configured.FRRPolicy.VRFs[0].PeerGroups[0].RemoteAS = &remoteAS
		Expect(check().State).To(Equal("unknown"))
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_ACTIVATION_UNKNOWN"))
	})
	It("reports missing mode, config and cross-source peer mismatch as unknown", func() {
		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(strings.Replace(configText, "datacenter", "traditional", 1))})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_MODE_UNKNOWN"))

		inv.HBN.Switches[0].Snapshot.Configured.FRRPolicy = nil
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_SOURCE_INCOMPLETE"))

		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(configText)})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("other")}})}}
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_PEER_BINDING_MISMATCH"))

		inv.HBN.Switches[0].Snapshot = fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(strings.Replace(configText, "interface peer-group", "peer-group", 1))})
		inv.HBN.Switches[0].Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: inv.HBN.Profile.FabricVRF, Neighbors: routingPolicyNeighbors([]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("fabric")}})}}
		Expect(check().ReasonCode).To(Equal("ROUTING_POLICY_PEER_TYPE_MISMATCH"))
	})
})

func routingPolicyNeighbors(neighbors []fabric.BGPNeighborConfig) *[]fabric.BGPNeighborConfig {
	for i := range neighbors {
		if neighbors[i].EffectiveRemoteAS == nil {
			remoteAS := "external"
			neighbors[i].EffectiveRemoteAS = &remoteAS
		}
	}
	return &neighbors
}
