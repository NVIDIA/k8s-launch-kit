// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package artifacts

import (
	"context"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifacts(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF artifacts") }

var _ = Describe("safe publication", func() {
	var dir string
	var input config.Input
	BeforeEach(func() {
		var e error
		dir, e = os.MkdirTemp("", "dpf-artifacts-")
		Expect(e).NotTo(HaveOccurred())
		dir, e = filepath.EvalSymlinks(dir)
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		source := []byte("# site intent\nmanagement:\n  kubeconfigRef: {file: credentials/mgmt}\ncredentials: {}\ndiscovery:\n  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: r}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: r}]\n")
		p := filepath.Join(dir, "input.yaml")
		Expect(os.WriteFile(p, source, 0600)).To(Succeed())
		input, e = config.Load(p)
		Expect(e).NotTo(HaveOccurred())
	})
	report := func(complete bool) *inventory.Report {
		outcome := "partial"
		if complete {
			outcome = "complete"
		}
		return &inventory.Report{Inventory: &inventory.Inventory{FormatVersion: "v1alpha1"}, Discovery: inventory.Discovery{Outcome: outcome}, Artifacts: []inventory.Artifact{}}
	}
	It("preserves normal handoff on partial collection and rebases references", func() {
		s, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		r := report(false)
		Expect(s.Publish(context.Background(), r)).To(Succeed())
		_, e = os.Stat(s.Output)
		Expect(os.IsNotExist(e)).To(BeTrue())
		b, e := os.ReadFile(filepath.Join(s.RunDir, "partial.yaml"))
		Expect(e).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring("# site intent"))
		Expect(string(b)).To(ContainSubstring("../../credentials/mgmt"))
		st, e := os.Stat(filepath.Join(s.RunDir, "report.json"))
		Expect(e).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()).To(Equal(os.FileMode(0600)))
	})
	It("creates an in-place source backup before replacement", func() {
		s, e := New(input, input.Path, true)
		Expect(e).NotTo(HaveOccurred())
		Expect(s.Publish(context.Background(), report(true))).To(Succeed())
		b, e := os.ReadFile(filepath.Join(s.RunDir, "source-before.yaml"))
		Expect(e).NotTo(HaveOccurred())
		Expect(b).To(Equal(input.Source))
	})
	It("rejects concurrent source edits before replacing output", func() {
		s, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		Expect(os.WriteFile(input.Path, []byte("changed"), 0600)).To(Succeed())
		Expect(s.Publish(context.Background(), report(true))).NotTo(Succeed())
		_, e = os.Stat(s.Output)
		Expect(os.IsNotExist(e)).To(BeTrue())
	})
	It("rejects existing unrelated output and symlinked run directories", func() {
		out := filepath.Join(dir, "out.yaml")
		Expect(os.WriteFile(out, []byte("unrelated: true"), 0600)).To(Succeed())
		_, e := New(input, out, true)
		Expect(e).To(HaveOccurred())
		Expect(os.Remove(out)).To(Succeed())
		Expect(os.Symlink(dir, filepath.Join(dir, "out.discovery"))).To(Succeed())
		_, e = New(input, out, true)
		Expect(e).To(HaveOccurred())
	})
	It("keeps a stable lock and detects a second cooperating writer", func() {
		a, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		b, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		Expect(a.Publish(context.Background(), report(true))).To(Succeed())
		Expect(b.Publish(context.Background(), report(true))).NotTo(Succeed())
		_, e = os.Stat(a.Output + ".lock")
		Expect(e).NotTo(HaveOccurred())
	})
	It("checks the destination baseline even when source already contains inventory", func() {
		output := filepath.Join(dir, "prior.yaml")
		prior := &inventory.Inventory{FormatVersion: "v1alpha1", ScopeDigest: "cluster-B"}
		saved, e := config.Saved(input, prior, output)
		Expect(e).NotTo(HaveOccurred())
		Expect(os.WriteFile(output, saved, 0600)).To(Succeed())
		input.Site.Inventory = &inventory.Inventory{FormatVersion: "v1alpha1", ScopeDigest: "cluster-A"}
		store, e := New(input, output, true)
		Expect(e).NotTo(HaveOccurred())
		r := report(true)
		r.Inventory.ScopeDigest = "cluster-A"
		Expect(store.Publish(context.Background(), r)).NotTo(Succeed())
		unchanged, e := os.ReadFile(output)
		Expect(e).NotTo(HaveOccurred())
		Expect(unchanged).To(Equal(saved))
	})

})
