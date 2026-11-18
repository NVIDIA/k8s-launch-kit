// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("two-file external inputs", func() {
	var dir, inventoryPath, overridesPath string
	write := func(path, text string) {
		Expect(os.WriteFile(path, []byte(text), 0600)).To(Succeed())
	}
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		inventoryPath, overridesPath = filepath.Join(dir, "inventory.yaml"), filepath.Join(dir, "overrides.yaml")
		write(inventoryPath, seed)
	})

	It("loads an inventory without overrides or reading unavailable access references", func() {
		input, err := LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(input.External).To(BeTrue())
		Expect(HasConfiguration(input)).To(BeFalse())
		Expect(input.Source).To(Equal([]byte(seed)))
		Expect(input.Path).To(Equal(inventoryPath))
		Expect(input.Site.Management.KubeconfigRef.File).To(Equal("credentials/mgmt"))
		Expect(input.Site.Provisioning).To(BeNil())
		Expect(input.Site.DPF.UseCase).To(BeNil())
	})

	It("treats null retained configuration as omitted, while preserving an explicit empty mapping", func() {
		write(inventoryPath, seed+"configuration: null\n")
		input, err := LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(HasConfiguration(input)).To(BeFalse())
		write(inventoryPath, seed+"configuration: {}\n")
		input, err = LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(HasConfiguration(input)).To(BeTrue())
	})

	It("composes environment allocations and typed overrides without raw provisioning", func() {
		write(inventoryPath, seed+`tenant:
  vip: 192.0.2.40
  interface: mgmt0
  allocation: {vipReserved: false, vipExcludedFromDHCP: true}
`)
		write(overridesPath, `useCase: hbn-bgp
release: 26.8.0
hbn:
  networking: {fabricVRF: tenant-vrf, fabricASN: 65002, dpuASNStride: 0}
  memory: 8Gi
  loopbackCIDR: 10.16.0.0/24
cluster: {routerID: 0, nodeSelector: {}}
`)
		input, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(input.Site.DPF.HBN.FabricVRF).To(HaveValue(Equal("tenant-vrf")))
		Expect(input.Site.DPF.HBN.DPUASNStride).To(HaveValue(BeZero()))
		Expect(input.Site.Provisioning.Bootstrap.Tenant.Allocation.VIPReserved).To(HaveValue(BeFalse()))
		Expect(input.Site.Provisioning.Bootstrap.Tenant.RouterID).To(HaveValue(BeZero()))
		Expect(input.Site.Provisioning.Bootstrap.Tenant.NodeSelector).To(HaveValue(BeEmpty()))
		Expect(input.Site.Provisioning.Resources).To(Equal(map[string]string{"memory": "8Gi"}))
		Expect(input.Site.Provisioning.Flavor).To(BeNil())
		Expect(input.Site.Provisioning.Software).To(Equal(Software{}))
		Expect(input.OverridesPath).To(Equal(overridesPath))
		Expect(input.OverridesSource).To(ContainSubstring("memory: 8Gi"))
		_, err = ResolveIntent(input.Site)
		Expect(err).To(HaveOccurred(), "explicit zero still must undergo downstream semantic validation")
	})

	DescribeTable("rejects implementation, software composition and wrong input types", func(contents string, inInventory bool, field string) {
		path := overridesPath
		if inInventory {
			path = inventoryPath
		}
		write(path, contents)
		if inInventory {
			path = ""
		}
		_, err := LoadInputs(inventoryPath, path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(field))
		Expect(err.Error()).NotTo(ContainSubstring("secret-canary"))
	},
		Entry("legacy site", "provisioning: {flavor: {nvconfig: secret-canary}}", true, "/provisioning"),
		Entry("raw override flavor", "flavor: {nvconfig: secret-canary}", false, "/flavor"),
		Entry("retained forbidden settings", "configuration: {software: {osISO: secret-canary}}", true, "/configuration/software"),
		Entry("component digest", "hbn: {imageDigest: secret-canary}", false, "/hbn/imageDigest"),
		Entry("lifecycle policy", "cluster: {bmcFactoryResetPolicy: secret-canary}", false, "/cluster/bmcFactoryResetPolicy"),
		Entry("raw resource map", "hbn: {resources: {custom/device: secret-canary}}", false, "/hbn/resources"),
		Entry("inventory access in overrides", "credentials: {root: {password: secret-canary}}", false, "/credentials"),
		Entry("inline password", "credentials: {root: {username: root, password: secret-canary}}", true, "/credentials/root/password"),
		Entry("numeric string", "hbn: {networking: {fabricASN: '65000'}}", false, "/hbn/networking/fabricASN"),
		Entry("string coercion", "hbn: {memory: 42}", false, "/hbn/memory"),
		Entry("tool probe image even null", "discovery: {probes: {imageRepository: null}}", true, "/discovery/probes/imageRepository"),
	)

	It("uses retained allowed configuration only when an overrides file is omitted", func() {
		write(inventoryPath, seed+"configuration: {useCase: hbn-bgp, namespace: custom-ns, hbn: {networking: {fabricVRF: retained}}}\n")
		input, err := LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(HasConfiguration(input)).To(BeTrue())
		Expect(input.Site.DPF.Namespace).To(HaveValue(Equal("custom-ns")))
		write(overridesPath, "{}\n")
		replaced, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(replaced.Site.DPF.UseCase).To(HaveValue(Equal("hbn-bgp")))
		Expect(replaced.Site.DPF.Namespace).To(BeNil())
		Expect(replaced.Site.DPF.HBN).To(BeNil())
		Expect(input.Site.DPF.HBN.FabricVRF).To(HaveValue(Equal("retained")))
	})

	It("rejects invalid HBN memory before resolving inaccessible credentials", func() {
		write(overridesPath, "useCase: hbn-bgp\nhbn: {memory: not-a-quantity}\n")
		input, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		_, err = Resolve(input)
		Expect(err).To(MatchError(ContainSubstring("HBN resource settings")))
	})

	It("keeps the selected use case when applying settings-only overrides to a handoff", func() {
		write(inventoryPath, seed+"configuration: {useCase: hbn-bgp}\n")
		write(overridesPath, "hbn: {memory: 8Gi}\n")
		input, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		intent, err := ResolveIntent(input.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(intent.UseCase).To(Equal("hbn-bgp"))
		Expect(input.Site.Provisioning.Resources).To(HaveKeyWithValue("memory", "8Gi"))
	})

	It("retains effective CLI selection and allowed configuration while omitting hydrated internals", func() {
		write(overridesPath, "hbn: {memory: 8Gi}\n")
		input, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		selection := "hbn-bgp"
		input, err = WithUseCase(input, &selection)
		Expect(err).NotTo(HaveOccurred())
		input.Site.Provisioning.Flavor = map[string]any{"ovs": "script-canary"}
		input.Site.Provisioning.Software = Software{OSISO: Artifact{URL: "https://example.test/internal.iso", SHA256: "digest-canary"}}
		Expect(input.Source).To(Equal([]byte(seed)))
		Expect(input.OverridesPath).To(Equal(overridesPath))
		Expect(input.OverridesSource).To(Equal([]byte("hbn: {memory: 8Gi}\n")))
		output, err := Saved(input, &inventory.Inventory{FormatVersion: inventory.FormatVersion}, filepath.Join(dir, "results", "inventory.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("# retained comment"))
		Expect(string(output)).To(ContainSubstring("../credentials/mgmt"))
		for _, forbidden := range []string{"provisioning:", "flavor:", "script-canary", "internal.iso", "digest-canary"} {
			Expect(string(output)).NotTo(ContainSubstring(forbidden))
		}
		saved, err := Decode(output, filepath.Join(dir, "results", "inventory.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(saved.External).To(BeTrue())
		Expect(saved.Site.DPF.UseCase).To(HaveValue(Equal("hbn-bgp")))
		Expect(saved.Site.Provisioning.Resources).To(Equal(map[string]string{"memory": "8Gi"}))
		Expect(saved.Site.Provisioning.Flavor).To(BeNil())
	})

	It("preserves declaration-file references independently of overrides location and rebase", func() {
		settingsDir := filepath.Join(dir, "settings")
		Expect(os.Mkdir(settingsDir, 0700)).To(Succeed())
		overridesPath = filepath.Join(settingsDir, "overrides.yaml")
		write(overridesPath, "useCase: hbn-bgp\n")
		// Keep the common management and credentials with a different SSH scope.
		write(inventoryPath, strings.Split(seed, "discovery:")[0]+`discovery:
  hosts:
    - ssh: {address: 192.0.2.10, knownHostsFile: trust/known_hosts, sudo: false}
      credentialRef: read
  dpus: []
`)
		input, err := LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		output, err := Saved(input, nil, filepath.Join(dir, "elsewhere", "out.yaml"))
		Expect(err).NotTo(HaveOccurred())
		var external ExternalInventory
		Expect(yaml.Unmarshal(output, &external)).To(Succeed())
		Expect(external.Management.KubeconfigRef.File).To(Equal("../credentials/mgmt"))
		Expect(external.Discovery.Hosts[0].SSH.KnownHostsFile).To(Equal("../trust/known_hosts"))
		Expect(external.Discovery.Hosts[0].SSH.Sudo).To(HaveValue(BeFalse()))
	})

	It("restores retained policy without replacing current environment allocations", func() {
		write(inventoryPath, seed+"tenant: {vip: 192.0.2.50, interface: mgmt0}\n")
		current, err := LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		name := "retained-tenant"
		result, err := WithConfiguration(current, Overrides{Cluster: &ClusterOverrides{TenantName: &name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Site.Provisioning.Bootstrap.Tenant.VIP).To(HaveValue(Equal("192.0.2.50")))
		Expect(result.Site.Provisioning.Bootstrap.Tenant.Name).To(HaveValue(Equal("retained-tenant")))
		Expect(current.Site.Provisioning.Bootstrap.Tenant.Name).To(BeNil())
		name = "changed"
		Expect(result.Site.Provisioning.Bootstrap.Tenant.Name).To(HaveValue(Equal("retained-tenant")))
	})

	It("requires recorded discovery settings and rejects effective policy changes", func() {
		input, err := LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(VerifyRecordedConfiguration(input)).To(MatchError(ContainSubstring("lacks recorded configuration")))
		write(inventoryPath, seed+"configuration: {useCase: hbn-bgp}\n")
		input, err = LoadInputs(inventoryPath, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(VerifyRecordedConfiguration(input)).To(Succeed())
		// Explicitly restating selected defaults is semantically equivalent.
		write(overridesPath, "useCase: hbn-bgp\nhbn: {networking: {fabricASN: 65000}}\n")
		input, err = LoadInputs(inventoryPath, overridesPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(VerifyRecordedConfiguration(input)).To(Succeed())
		for _, changed := range []string{
			"hbn: {networking: {fabricVRF: other}}",
			"hbn: {memory: 8Gi}",
			"cluster: {podCIDR: 10.48.0.0/16}",
			"cluster: {routerID: 127}",
		} {
			write(overridesPath, "useCase: hbn-bgp\n"+changed+"\n")
			input, err = LoadInputs(inventoryPath, overridesPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(VerifyRecordedConfiguration(input)).To(MatchError(ContainSubstring("differs from saved discovery")))
		}
	})
})
