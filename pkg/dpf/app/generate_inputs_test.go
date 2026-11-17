// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("DPF generation input boundary", func() {
	It("shares complete selected identities and ASNs with enrollment without aliasing inventory", func() {
		input := generationSite()
		intent, err := config.ResolveIntent(input.Site)
		Expect(err).NotTo(HaveOccurred())
		selected, err := ResolveDevices(input.Site, input.Site.Inventory, *intent.Networking)
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(HaveLen(1))
		Expect(selected[0].Serial).To(Equal("SERIAL-A"))
		Expect(selected[0].PSID).To(Equal("MT_0000001774"))
		Expect(selected[0].BMCIP).To(Equal("192.0.2.30"))
		Expect(selected[0].BMCPort).To(Equal(443))
		asns, err := intent.Networking.AllocateASNs([]string{"SERIAL-A"})
		Expect(err).NotTo(HaveOccurred())
		Expect(selected[0].ASN).To(Equal(asns["SERIAL-A"]))
		Expect(selected[0].SelectorLabels).NotTo(BeEmpty())
		plan, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Workload.Devices).To(HaveLen(1))
		Expect(plan.Operator.Bootstrap.Devices).To(HaveLen(1))
		Expect(plan.Workload.Devices[0].Serial).To(Equal(selected[0].Serial))
		Expect(plan.Workload.Devices[0].PSID).To(Equal(selected[0].PSID))
		Expect(plan.Workload.Devices[0].ASN).To(Equal(selected[0].ASN))
		Expect(plan.Workload.Devices[0].SelectorLabels).To(Equal(selected[0].SelectorLabels))
		Expect(plan.Operator.Bootstrap.Devices[0].BMCIP).To(Equal(selected[0].BMCIP))
		Expect(plan.Operator.Bootstrap.Devices[0].BMCPort).To(Equal(selected[0].BMCPort))
		selected[0].SelectorLabels["only-local"] = "changed"
		Expect(plan.Workload.Devices[0].SelectorLabels).NotTo(HaveKey("only-local"))
		Expect(input.Site.Inventory.DPUs[0].SerialNumber).To(Equal("SERIAL-A"))
	})

	It("resolves defaults and planned selectors without opening credentials or changing inputs", func() {
		input := generationSite()
		plan, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Operator.Bootstrap.ManagementAPIIP).To(Equal("192.0.2.10"))
		Expect(plan.Workload.Devices).To(HaveLen(1))
		Expect(plan.Workload.Devices[0].PSID).To(Equal("MT_0000001774"))
		Expect(plan.Workload.HBNResources).To(HaveKeyWithValue("memory", "6Gi"))
		Expect(plan.Workload.DPUClusterSelector).To(Equal(plan.Operator.Bootstrap.Tenant.Labels))
		Expect(plan.SourceConfig).To(Equal(input.Source))
		Expect(input.Site.Provisioning.Resources).To(BeNil())
	})
	It("isolates returned mutable desired inputs and preserves prior inputs and defaults", func() {
		input := generationSite()
		input.Site.Provisioning.Flavor["nested"] = map[string]any{"items": []any{map[string]any{"enabled": false, "count": 0, "empty": "", "omitted": nil}}, "emptyMap": map[string]any{}, "emptySlice": []any{}}
		before, err := yaml.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		first, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		after, err := yaml.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		nested := first.Workload.FlavorSpec["nested"].(map[string]any)
		Expect(nested["emptyMap"]).NotTo(BeNil())
		Expect(nested["emptySlice"]).NotTo(BeNil())
		first.SourceConfig[0] = '!'
		first.Workload.Software.PLDM["MT_0000001774"] = config.Artifact{}
		first.Workload.HBNResources["memory"] = "changed"
		first.Workload.Devices[0].SelectorLabels["changed"] = "true"
		first.Operator.Bootstrap.Tenant.Labels["changed"] = "true"
		nested["items"].([]any)[0].(map[string]any)["enabled"] = true
		nested["emptyMap"].(map[string]any)["changed"] = true
		after, err = yaml.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		second, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Workload.HBNResources["memory"]).To(Equal("6Gi"))
		Expect(second.Workload.FlavorSpec["nested"]).To(Equal(input.Site.Provisioning.Flavor["nested"]))
		Expect(second.Workload.DPUClusterSelector).NotTo(HaveKey("changed"))
	})
	It("rejects missing or contradictory selected identities before generation", func() {
		cases := []func(*config.Input){
			func(i *config.Input) { i.Site.Discovery.Hosts[0].SSH.Address = "192.0.2.41" },
			func(i *config.Input) { i.Site.Discovery.Hosts = nil },
			func(i *config.Input) {
				changed := "/redfish/v1/Systems/other"
				i.Site.Discovery.DPUs[0].SystemURI = &changed
			},
			func(i *config.Input) { i.Site.Inventory = nil },
			func(i *config.Input) {
				i.Site.Inventory.DPUs[0].Observations["bmc-administrator"] = inventory.Observation{State: "unknown", ReasonCode: "NOT_VERIFIED"}
			},
			func(i *config.Input) { i.Site.Inventory.Associations[0].HostAdapterKey = "other" },
			func(i *config.Input) { i.Site.Discovery.DPUs[0].BMCEndpoint = "https://192.0.2.31" },
			func(i *config.Input) { i.Site.Inventory.DPUs[0].InputRef = "/discovery/dpus/1" },
			func(i *config.Input) { i.Site.Inventory.DPUs = append(i.Site.Inventory.DPUs, i.Site.Inventory.DPUs[0]) },
			func(i *config.Input) { i.Site.Inventory.Software.Version = "26.4.0" },
			func(i *config.Input) { i.Site.Inventory.Management.SystemNamespaceUID = "" },
			func(i *config.Input) { i.Site.Provisioning.Flavor = nil },
			func(i *config.Input) { i.Site.Provisioning.Software.PLDM = nil },
			func(i *config.Input) { i.Site.Provisioning.Bootstrap.Tenant.VIP = nil },
			func(i *config.Input) { i.Site.Provisioning.Resources = map[string]string{} },
		}
		for _, mutate := range cases {
			input := generationSite()
			mutate(&input)
			_, err := ResolveGeneration(input, nil)
			Expect(err).To(HaveOccurred())
		}
	})
	It("binds an explicit host BMC system before accepting saved inventory", func() {
		input := generationSite()
		system := "/redfish/v1/Systems/1"
		input.Site.Discovery.Hosts[0] = config.Endpoint{BMCEndpoint: "https://192.0.2.40", SystemURI: &system, CredentialRef: "host"}
		input.Site.Inventory.Hosts[0].SSHEndpoint = ""
		input.Site.Inventory.Hosts[0].BMCEndpoint = "https://192.0.2.40"
		input.Site.Inventory.Hosts[0].SystemURI = system
		_, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		changed := "/redfish/v1/Systems/2"
		input.Site.Discovery.Hosts[0].SystemURI = &changed
		_, err = ResolveGeneration(input, nil)
		Expect(err).To(MatchError(ContainSubstring("system")))
	})
})
