// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN discovery publication", func() {
	It("publishes CLI use-case selection without mistaking the override for a concurrent source edit", func() {
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(dir, "site.yaml")
		Expect(os.WriteFile(path, []byte("# source stays unchanged\ndiscovery: {}\n"), 0600)).To(Succeed())
		input, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())
		selection := "hbn-bgp"
		input, err = config.WithUseCase(input, &selection)
		Expect(err).NotTo(HaveOccurred())
		store, err := New(input, "", false)
		Expect(err).NotTo(HaveOccurred())
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		report := &inventory.Report{Inventory: &inventory.Inventory{FormatVersion: inventory.FormatVersion, HBN: &inventory.HBNInventory{Profile: profile}}, Discovery: inventory.Discovery{Outcome: "complete"}, Readiness: &inventory.Readiness{UseCase: "hbn-bgp", State: "blocked"}}
		Expect(store.Publish(context.Background(), report)).To(Succeed())
		saved, err := config.Load(store.Output)
		Expect(err).NotTo(HaveOccurred())
		Expect(*saved.Site.DPF.UseCase).To(Equal(selection))
		Expect(saved.Site.Inventory.HBN.Profile).To(Equal(profile))
		original, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(original).To(Equal(input.Source))
	})
	It("compares effective HBN intent while allowing rebased trust reference paths", func() {
		input, err := config.Decode([]byte(`dpf: {useCase: hbn-bgp}
discovery:
  switches:
    - name: leaf-a
      ssh: {address: leaf.example, knownHostsFile: trust}
      credentialRef: read
      ports: [{name: swp1, dpuSerial: DPU-A, dpuInterface: p0}]
`), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		data, err := config.Saved(input, nil, "/site/results/output.yaml")
		Expect(err).NotTo(HaveOccurred())
		rebased, err := config.Decode(data, "/site/results/output.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(sameRequested(input.Site, rebased.Site)).To(BeTrue())
		namespace := "other-operator"
		rebased.Site.DPF.Namespace = &namespace
		Expect(sameRequested(input.Site, rebased.Site)).To(BeFalse())
		namespace = "dpf-operator-system"
		Expect(sameRequested(input.Site, rebased.Site)).To(BeTrue())
		rebased.Site.Discovery.Switches[0].Ports[0].DPUInterface = "p1"
		Expect(sameRequested(input.Site, rebased.Site)).To(BeFalse())
	})
	It("protects a switch known-hosts reference from output aliasing", func() {
		dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(dir, "site.yaml")
		Expect(os.WriteFile(path, []byte("discovery: {}\n"), 0600)).To(Succeed())
		input, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())
		input.Site.Discovery.Switches = []config.Switch{{SSH: config.SSH{KnownHostsFile: "trust"}}}
		_, err = New(input, filepath.Join(dir, "trust"), true)
		Expect(err).To(MatchError("output aliases a private reference"))
	})
})
