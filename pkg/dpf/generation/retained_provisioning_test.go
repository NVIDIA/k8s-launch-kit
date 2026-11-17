// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

func TestRetainedProvisioningAndConfigurationReturnIndependentOwnership(t *testing.T) {
	input := generationInputs(t)
	input.Workload.HBNResources["nvidia.com/bf_sf"] = "8"
	input.Operator.Bootstrap.ArtifactPVC = "retained-artifacts"
	input.Workload.Generation.WorkloadGatewayIndex = 3
	input.Workload.Generation.HostAddressIndex = 4
	retained, err := Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := LoadPlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	memory, loopback := "8Gi", "10.16.0.0/24"
	selector := map[string]string{"selected": "true"}
	current := config.Site{Provisioning: &config.Provisioning{
		Resources: map[string]string{"memory": memory}, HBN: &hbn.GenerationOptions{LoopbackCIDR: &loopback},
		Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{NodeSelector: &selector}},
	}}
	fixed, err := plan.ResolveProvisioning(current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixed.Software, input.Workload.Software) || !reflect.DeepEqual(fixed.Flavor, input.Workload.FlavorSpec) {
		t.Fatal("fixed recipe differs from the validated retained inputs")
	}
	if fixed.Resources["memory"] != memory || fixed.Resources["nvidia.com/bf_sf"] != "8" || *fixed.HBN.LoopbackCIDR != loopback || *fixed.HBN.WorkloadGatewayIndex != 3 || *fixed.HBN.HostAddressIndex != 4 || *fixed.Bootstrap.ArtifactPVC != "retained-artifacts" {
		t.Fatal("retained fixed inputs or current supported settings were replaced")
	}
	settings, err := plan.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	if *settings.Release != plan.Intent().Release || *settings.HBN.Memory != input.Workload.HBNResources["memory"] || *settings.Cluster.RouterID != input.Operator.Bootstrap.Tenant.RouterID {
		t.Fatal("supported configuration must reflect effective retained values")
	}
	fixed.Flavor["dpuMode"] = "mutated"
	fixed.Software.PLDM["MT_0000001774"] = structArtifact()
	fixed.Resources["memory"] = "changed"
	(*fixed.Bootstrap.Tenant.NodeSelector)["selected"] = "changed"
	*fixed.HBN.LoopbackCIDR = "changed"
	*settings.HBN.Networking.WorkloadMTU = 1400
	(*settings.Cluster.NodeSelector)["changed"] = "true"
	again, err := plan.ResolveProvisioning(current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Flavor, input.Workload.FlavorSpec) || !reflect.DeepEqual(again.Software, input.Workload.Software) {
		t.Fatal("fixed recipe accessor returned aliases into the retained plan")
	}
	if again.Resources["memory"] != memory || *again.HBN.LoopbackCIDR != loopback || selector["selected"] != "true" {
		t.Fatal("retained resolution mutated current inputs or shared result storage")
	}
	againSettings, err := plan.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	if *againSettings.HBN.Networking.WorkloadMTU != input.Workload.Profile.WorkloadMTU || (*againSettings.Cluster.NodeSelector)["changed"] != "" {
		t.Fatal("configuration accessor returned aliases into the retained plan")
	}
}
