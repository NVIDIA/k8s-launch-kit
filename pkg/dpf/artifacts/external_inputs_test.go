// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("two-file inventory publication", func() {
	var dir, source, overrides string
	var input config.Input
	BeforeEach(func() {
		var err error
		dir, err = filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		source, overrides = filepath.Join(dir, "inventory.yaml"), filepath.Join(dir, "overrides.yaml")
		Expect(os.WriteFile(source, []byte("# environment intent\ncredentials: {}\ndiscovery: {hosts: [], dpus: []}\n"), 0600)).To(Succeed())
		Expect(os.WriteFile(overrides, []byte("useCase: hbn-bgp\nhbn: {memory: 8Gi}\n"), 0600)).To(Succeed())
		input, err = config.LoadInputs(source, overrides)
		Expect(err).NotTo(HaveOccurred())
	})

	It("fails publication when either source changes and preserves the prior output", func() {
		for _, changed := range []string{source, overrides} {
			original, err := os.ReadFile(changed)
			Expect(err).NotTo(HaveOccurred())
			output := filepath.Join(dir, filepath.Base(changed)+".output")
			store, err := New(input, output, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(changed, append(original, []byte("# concurrent change\n")...), 0600)).To(Succeed())
			report := &inventory.Report{Inventory: &inventory.Inventory{FormatVersion: inventory.FormatVersion}, Discovery: inventory.Discovery{Outcome: "complete"}}
			Expect(store.Publish(context.Background(), report)).NotTo(Succeed())
			_, err = os.Stat(output)
			Expect(os.IsNotExist(err)).To(BeTrue())
			Expect(os.WriteFile(changed, original, 0600)).To(Succeed())
		}
	})

	It("rejects output overwriting the optional overrides input or its hardlink", func() {
		_, err := New(input, overrides, true)
		Expect(err).To(HaveOccurred())
		alias := filepath.Join(dir, "overrides-alias.yaml")
		Expect(os.Link(overrides, alias)).To(Succeed())
		_, err = New(input, alias, true)
		Expect(err).To(HaveOccurred())
		contents, err := os.ReadFile(overrides)
		Expect(err).NotTo(HaveOccurred())
		Expect(contents).To(Equal(input.OverridesSource))
	})

	It("also protects an overrides file that aliases an explicitly refreshed source", func() {
		Expect(os.WriteFile(source, []byte("{}\n"), 0600)).To(Succeed())
		var err error
		input, err = config.LoadInputs(source, source)
		Expect(err).NotTo(HaveOccurred())
		_, err = New(input, source, true)
		Expect(err).To(MatchError(ContainSubstring("private reference")))
	})

	It("publishes only external inventory and pinned allowed intent, then refreshes that handoff", func() {
		output := filepath.Join(dir, "discovered.yaml")
		store, err := New(input, output, true)
		Expect(err).NotTo(HaveOccurred())
		input.Site.Provisioning.Flavor = map[string]any{"ovs": "raw-script-canary"}
		// Ensure publication projects from the effective input, not a raw Site dump.
		store.Input = input
		report := &inventory.Report{Inventory: &inventory.Inventory{FormatVersion: inventory.FormatVersion}, Discovery: inventory.Discovery{Outcome: "complete"}}
		Expect(store.Publish(context.Background(), report)).To(Succeed())
		contents, err := os.ReadFile(output)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(contents)).To(ContainSubstring("# environment intent"))
		Expect(string(contents)).To(ContainSubstring("configuration:"))
		Expect(string(contents)).NotTo(ContainSubstring("provisioning:"))
		Expect(string(contents)).NotTo(ContainSubstring("raw-script-canary"))
		reloaded, err := config.LoadInputs(output, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(config.HasConfiguration(reloaded)).To(BeTrue())
		Expect(reloaded.Site.DPF.Release).NotTo(BeNil())
		Expect(reloaded.Site.DPF.HBN.FabricASN).NotTo(BeNil())
		Expect(reloaded.Site.Provisioning.Bootstrap.Tenant.RouterID).NotTo(BeNil())
		_, err = New(reloaded, output, true)
		Expect(err).NotTo(HaveOccurred())
	})
})
