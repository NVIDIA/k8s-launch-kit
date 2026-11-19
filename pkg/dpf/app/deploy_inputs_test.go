// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func deploymentSite() config.Input {
	input := generationSite()
	input.Site.Inventory.ScopeDigest = "same-scope"
	input.Site.Inventory.Hosts[0].RecordKey = "host-source"
	input.Site.Inventory.DPUs[0].RecordKey = "dpu-source"
	return input
}

var _ = Describe("DPF deployment input binding", func() {
	It("rejects a structurally valid rehashed altered bundle before access or report creation", func() {
		_, retained := generatedUseCaseSite(deploymentSite())
		index, files := retained.Index(), retained.Files()
		index.BundleDigest = ""
		for i := range index.Files {
			index.Files[i].SHA256 = ""
		}
		changed := false
		for i := range files {
			for _, record := range index.Files {
				if record.Path == files[i].Path && record.Role == bundle.Projection {
					files[i].Data = append(files[i].Data, []byte("\n# unreviewed projection change\n")...)
					changed = true
				}
			}
		}
		Expect(changed).To(BeTrue())
		tampered, err := bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(root, "retained")
		_, err = tampered.Publish(context.Background(), dir)
		Expect(err).NotTo(HaveOccurred())
		// A later configuration/access read would fail differently; the Plan
		// constructor must reject first, before allocating any report directory.
		result, err := Deploy(context.Background(), DeploymentRequest{DeploymentFiles: dir, Inventory: filepath.Join(root, "must-not-open")})
		Expect(result).To(BeNil())
		var preparation *PreparationError
		Expect(errors.As(err, &preparation)).To(BeTrue())
		Expect(preparation.Stage).To(Equal(DeploymentPlan))
		reports, err := filepath.Glob(filepath.Join(root, "l8k-dpf-deploy-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reports).To(BeEmpty())
	})

	It("rejects changed physical hosts and associations", func() {
		t := GinkgoT()
		before := deploymentSite().Site.Inventory
		before.ScopeDigest = "same-scope"
		before.Hosts[0].SystemUUID = "host-system-uuid"
		encoded, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			change func(*inventory.Inventory)
			valid  bool
		}{
			{name: "unchanged", change: func(*inventory.Inventory) {}, valid: true},
			{name: "host replaced", change: func(i *inventory.Inventory) { i.Hosts[0].SystemUUID = "replacement" }},
			{name: "endpoint changed", change: func(i *inventory.Inventory) { i.Hosts[0].SSHEndpoint = "ssh://192.0.2.99:22" }},
			{name: "trust changed", change: func(i *inventory.Inventory) { i.ScopeDigest = "other-scope" }},
			{name: "adapter changed", change: func(i *inventory.Inventory) { i.Associations[0].HostAdapterKey = "other-adapter" }},
			{name: "duplicate host", change: func(i *inventory.Inventory) { i.Hosts = append(i.Hosts, i.Hosts[0]) }},
			{name: "evidence omitted", change: func(i *inventory.Inventory) { i.Evidence = nil; i.Hosts[0].EvidenceRefs = nil }, valid: true},
		} {
			By(test.name)
			{
				var current inventory.Inventory
				if err := json.Unmarshal(encoded, &current); err != nil {
					t.Fatal(err)
				}
				test.change(&current)
				if err := config.VerifyRetainedPhysicalBindings(before, &current); (err == nil) != test.valid {
					t.Fatalf("valid=%v, got %v", test.valid, err)
				}
			}
		}
	})

	It("keeps current catalog generation separate from retained recipes without changing public inputs", func() {
		loaded, retained := generatedUseCaseSite(deploymentSite())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(loaded.Path, publicFixtureBytes(loaded), 0600)).To(Succeed())
		input, err := config.LoadInputs(loaded.Path, "")
		Expect(err).NotTo(HaveOccurred())
		before, err := json.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		fresh, err := ResolveGeneration(input, nil)
		Expect(err).NotTo(HaveOccurred())
		fixed, err := ResolveGeneration(input, plan)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh.Workload.Software.OSISO).NotTo(Equal(fixed.Workload.Software.OSISO))
		Expect(fixed.Workload.Software).To(Equal(plan.Intent().Software))
		Expect(fixed.Workload.FlavorSpec).To(HaveKeyWithValue("dpuMode", "dpu"))
		Expect(VerifyDeploymentPlan(input, retained, plan)).To(Succeed())
		after, err := json.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))

		input.Site.Discovery.Hosts[0].SSH.Address = "192.0.2.99"
		Expect(VerifyDeploymentPlan(input, retained, plan)).To(MatchError(ContainSubstring("SSH source changed")))
		input.Site.Discovery.Hosts[0].SSH.Address = "192.0.2.40"
		input.Site.Inventory.Management.SystemNamespaceUID = "replacement-cluster"
		Expect(VerifyDeploymentPlan(input, retained, plan)).To(MatchError(ContainSubstring("generated plan")))
	})

	It("rejects changed intent without opening access", func() {
		t := GinkgoT()
		input := deploymentSite()
		input.Site.Inventory.ScopeDigest = "same-scope"
		data, err := yaml.Marshal(input.Site)
		if err != nil {
			t.Fatal(err)
		}
		input, err = config.Decode(data, "/site/site.yaml")
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := ResolveGeneration(input, nil)
		if err != nil {
			t.Fatal(err)
		}
		resolved.OperatorArchive, err = os.ReadFile("../bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
		if err != nil {
			t.Fatal(err)
		}
		resolved.HBNChartArchive, err = os.ReadFile("../generation/testdata/doca-hbn-3.5.0.tgz")
		if err != nil {
			t.Fatal(err)
		}
		retained, err := generation.Generate(resolved.Inputs)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := generation.LoadPlan(retained)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyDeploymentPlan(input, retained, plan); err != nil {
			t.Fatal(err)
		}
		input.Site.Provisioning.Flavor["dpuMode"] = "nic"
		if err := VerifyDeploymentPlan(input, retained, plan); err == nil {
			t.Fatal("changed desired flavor was accepted against the retained bundle")
		}
	})
})
