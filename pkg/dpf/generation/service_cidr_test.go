// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"gopkg.in/yaml.v3"
)

func TestGenerateRejectsCIDRThatDiffersFromItsSelectedController(t *testing.T) {
	for _, value := range []string{"", "172.31.0.0/16", "10.96.1.0/16"} {
		input := generationInputs(t)
		input.Operator.Bootstrap.Tenant.ServiceCIDR = value
		before := cloneInputs(input)
		if _, err := Generate(input); err == nil || !strings.Contains(err.Error(), "controller contract") {
			t.Fatalf("direct Generate CIDR %q accepted: %v", value, err)
		}
		if !reflect.DeepEqual(input, before) {
			t.Fatal("rejected Generate changed input")
		}
	}
}

func TestLoadPlanDoesNotRepairChangedRetainedServiceCIDR(t *testing.T) {
	base, err := Generate(generationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "172.31.0.0/16"} {
		mutated := rehashBundle(t, base, func(_ *bundle.Index, files *[]bundle.File) {
			mutateFile(t, files, bundle.ResolvedConfigPath, func(raw []byte) []byte {
				var root map[string]any
				if err := yaml.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				root["config"].(map[string]any)["bootstrap"].(map[string]any)["tenant"].(map[string]any)["serviceCIDR"] = value
				return marshalYAML(t, root)
			})
		})
		before := mutated.Index().BundleDigest
		beforeFiles := mutated.Files()
		if _, err := LoadPlan(mutated); err == nil {
			t.Fatal("LoadPlan adopted/repaired retained serviceCIDR")
		}
		if mutated.Index().BundleDigest != before || !reflect.DeepEqual(beforeFiles, mutated.Files()) {
			t.Fatal("LoadPlan mutated retained input")
		}
	}
}
