// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"encoding/json"
	"os"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
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
				if err := verifyDeploymentPhysicalBindings(before, &current); (err == nil) != test.valid {
					t.Fatalf("valid=%v, got %v", test.valid, err)
				}
			}
		}
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
		resolved, err := generationInputs(input)
		if err != nil {
			t.Fatal(err)
		}
		resolved.OperatorArchive, err = os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
		if err != nil {
			t.Fatal(err)
		}
		resolved.HBNChartArchive, err = os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
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
		if err := verifyDeploymentPlan(input, retained, plan); err != nil {
			t.Fatal(err)
		}
		input.Site.Provisioning.Flavor["dpuMode"] = "nic"
		if err := verifyDeploymentPlan(input, retained, plan); err == nil {
			t.Fatal("changed desired flavor was accepted against the retained bundle")
		}
	})
})
