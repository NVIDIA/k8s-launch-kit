// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbn

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	dpfassets "github.com/nvidia/k8s-launch-kit/profiles"
	"gopkg.in/yaml.v3"
)

func TestResolveDefaultsAndYAMLNull(t *testing.T) {
	definition, err := LoadDefinition()
	if err != nil {
		t.Fatalf("LoadDefinition() error = %v", err)
	}
	var options Options
	if err := yaml.Unmarshal([]byte("fabricVRF: null\nfabricASN: null\n"), &options); err != nil {
		t.Fatalf("decode omitted/null options: %v", err)
	}
	profile, err := Resolve(options)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := Profile{
		FabricVRF:      "default",
		FabricASN:      65000,
		DPUASNBase:     65101,
		DPUASNStride:   100,
		UplinkMTU:      9216,
		WorkloadMTU:    9000,
		WorkloadCIDR:   "10.0.120.0/22",
		WorkloadPrefix: 29,
	}
	if definition.Defaults.Networking != want {
		t.Fatalf("embedded networking defaults = %#v, want %#v", definition.Defaults.Networking, want)
	}
	if profile != want {
		t.Fatalf("Resolve() = %#v, want %#v", profile, want)
	}
}

func TestLoadDefinitionMetadataAndEmbeddedDefaults(t *testing.T) {
	definition, err := LoadDefinition()
	if err != nil {
		t.Fatalf("LoadDefinition() error = %v", err)
	}
	if definition.Plugin != "dpf" || definition.Name != "hbn-bgp" || definition.Target != "dpf" || definition.Description == "" {
		t.Fatalf("unexpected profile metadata: %#v", definition)
	}
	if definition.Defaults.Release != "26.8.0" || definition.Defaults.Namespace != "dpf-operator-system" {
		t.Fatalf("unexpected profile release/namespace defaults: %#v", definition.Defaults)
	}
	if got := definition.Defaults.Generation; got.ChartName != "doca-hbn" || got.ChartVersion != "3.5.0" || got.ImageTag != "3.5.0-doca3.5.0" || got.StartupVersion != "HBN 2.4.0" || got.NodeEffect != NodeEffectHold || got.DPUSetStrategy != DPUSetStrategyOnDelete || got.VXLANStartupProbeEnabled {
		t.Fatalf("unexpected generation defaults: %#v", got)
	}
	bootstrap := definition.Defaults.Bootstrap
	if bootstrap.OperatorConfigName != "dpfoperatorconfig" || bootstrap.BMCFactoryResetPolicy != "Never" || bootstrap.TenantName != "dpu-cplane-tenant1" || bootstrap.TenantNamespace != "dpu-cplane-tenant1" || bootstrap.TenantMaxNodes != 1000 || bootstrap.TenantRouterID != 126 || bootstrap.PodCIDR != "10.244.0.0/14" || bootstrap.TenantNodeSelector["node-role.kubernetes.io/control-plane"] != "" {
		t.Fatalf("unexpected bootstrap defaults: %#v", bootstrap)
	}
	if got := definition.Defaults.Resources; !reflect.DeepEqual(got, Resources{"memory": "6Gi", "nvidia.com/bf_sf": "4"}) {
		t.Fatalf("unexpected HBN resource defaults: %#v", got)
	}
	asset := dpfassets.DPFHBNBGPProfile()
	asset[0] = 'x'
	if again, err := LoadDefinition(); err != nil || again.Name != "hbn-bgp" {
		t.Fatalf("mutating returned asset affected embedded definition: %#v, %v", again, err)
	}
}

func TestValidateResources(t *testing.T) {
	tests := []struct {
		name      string
		resources Resources
		wantError bool
	}{
		{name: "positive standard and extended quantities", resources: Resources{"cpu": "250m", "memory": "6Gi", "nvidia.com/bf_sf": "4"}},
		{name: "empty map", resources: Resources{}, wantError: true},
		{name: "invalid resource name", resources: Resources{"bad/name/extra": "1"}, wantError: true},
		{name: "unknown unqualified resource", resources: Resources{"banana": "1"}, wantError: true},
		{name: "reserved kubernetes extended resource", resources: Resources{"kubernetes.io/example": "1"}, wantError: true},
		{name: "malformed quantity", resources: Resources{"memory": "many"}, wantError: true},
		{name: "zero quantity", resources: Resources{"nvidia.com/bf_sf": "0"}, wantError: true},
		{name: "negative quantity", resources: Resources{"cpu": "-1"}, wantError: true},
		{name: "fractional extended resource", resources: Resources{"nvidia.com/bf_sf": "500m"}, wantError: true},
		{name: "integer extended resource quantity", resources: Resources{"nvidia.com/bf_sf": "1000m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateResources(tt.resources)
			if (err != nil) != tt.wantError {
				t.Fatalf("ValidateResources() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestResolveGenerationDefaultsOverridesAndNull(t *testing.T) {
	var omitted GenerationOptions
	if err := yaml.Unmarshal([]byte("chartDigest: null\nworkloadGatewayIndex: null\nvxlanStartupProbeEnabled: null\n"), &omitted); err != nil {
		t.Fatalf("decode null generation options: %v", err)
	}
	generation, err := ResolveGeneration(omitted)
	if err != nil {
		t.Fatalf("ResolveGeneration() error = %v", err)
	}
	want := Generation{
		ChartRepository:          "https://helm.ngc.nvidia.com/nvidia/doca",
		ChartName:                "doca-hbn",
		ChartVersion:             "3.5.0",
		ChartDigest:              "sha256:85dc3b44307b92088fc3d05561e66cada4a7d8390ff6ff3e6c6fde8f9c5a42d9",
		ImageRepository:          "nvcr.io/nvidia/doca/doca_hbn",
		ImageTag:                 "3.5.0-doca3.5.0",
		ImageDigest:              "sha256:675ff5bf03acf5e2c777309d9dbe570754d3348be661278df094783f18962d93",
		StartupVersion:           "HBN 2.4.0",
		LoopbackCIDR:             "10.0.124.0/24",
		WorkloadGatewayIndex:     1,
		DPUSetStrategy:           DPUSetStrategyOnDelete,
		NodeEffect:               NodeEffectHold,
		VXLANStartupProbeEnabled: false,
	}
	if generation != want {
		t.Fatalf("default generation = %#v, want %#v", generation, want)
	}
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := ValidateGeneration(profile, generation); err != nil {
		t.Fatalf("ValidateGeneration(defaults) error = %v", err)
	}

	noEffect, rolling, enabledProbe, gateway := NodeEffectNoEffect, DPUSetStrategyRollingUpdate, true, 2
	chartRepository, chartName, chartVersion := "https://mirror.example.net/charts", "doca-hbn-custom", "3.6.0"
	chartDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imageRepository, imageTag := "registry.example.net/team/hbn", "custom-build.1"
	imageDigest, startupVersion, loopbackCIDR := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "HBN 2.5.0", "192.168.0.0/24"
	overridden, err := ResolveGeneration(GenerationOptions{
		ChartRepository:          &chartRepository,
		ChartName:                &chartName,
		ChartVersion:             &chartVersion,
		ChartDigest:              &chartDigest,
		ImageRepository:          &imageRepository,
		ImageTag:                 &imageTag,
		ImageDigest:              &imageDigest,
		StartupVersion:           &startupVersion,
		LoopbackCIDR:             &loopbackCIDR,
		WorkloadGatewayIndex:     &gateway,
		DPUSetStrategy:           &rolling,
		NodeEffect:               &noEffect,
		VXLANStartupProbeEnabled: &enabledProbe,
	})
	if err != nil {
		t.Fatalf("ResolveGeneration(overrides) error = %v", err)
	}
	if overridden.ChartRepository != chartRepository || overridden.ChartName != chartName || overridden.ChartVersion != chartVersion || overridden.ChartDigest != chartDigest || overridden.ImageRepository != imageRepository || overridden.ImageTag != imageTag || overridden.ImageDigest != imageDigest || overridden.StartupVersion != startupVersion || overridden.LoopbackCIDR != loopbackCIDR || overridden.WorkloadGatewayIndex != gateway || overridden.DPUSetStrategy != rolling || overridden.NodeEffect != noEffect || !overridden.VXLANStartupProbeEnabled {
		t.Fatalf("generation overrides lost explicit values: %#v", overridden)
	}
	if err := ValidateGeneration(profile, overridden); err != nil {
		t.Fatalf("ValidateGeneration(overrides) error = %v", err)
	}
}

func TestResolveGenerationRejectsExplicitEmptyZeroAndMalformedValues(t *testing.T) {
	empty, zero := "", 0
	badDigest := "sha256:ABC"
	badNodeEffect, badStrategy := NodeEffect(""), DPUSetStrategy("ApplyImmediately")
	tests := []struct {
		name    string
		options GenerationOptions
	}{
		{name: "empty chart repository", options: GenerationOptions{ChartRepository: &empty}},
		{name: "empty chart name", options: GenerationOptions{ChartName: &empty}},
		{name: "floating chart version", options: GenerationOptions{ChartVersion: stringPointer("3.5.x")}},
		{name: "invalid chart digest", options: GenerationOptions{ChartDigest: &badDigest}},
		{name: "invalid image digest", options: GenerationOptions{ImageDigest: &badDigest}},
		{name: "image reference includes tag syntax", options: GenerationOptions{ImageRepository: stringPointer("nvcr.io/nvidia/doca/doca_hbn:latest")}},
		{name: "zero gateway index", options: GenerationOptions{WorkloadGatewayIndex: &zero}},
		{name: "empty loopback pool", options: GenerationOptions{LoopbackCIDR: &empty}},
		{name: "invalid node effect", options: GenerationOptions{NodeEffect: &badNodeEffect}},
		{name: "invalid update strategy", options: GenerationOptions{DPUSetStrategy: &badStrategy}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveGeneration(test.options); err == nil {
				t.Fatal("ResolveGeneration() succeeded; want validation error")
			}
		})
	}

	var decoded GenerationOptions
	if err := yaml.Unmarshal([]byte("chartName: ''\nworkloadGatewayIndex: 0\nvxlanStartupProbeEnabled: true\n"), &decoded); err != nil {
		t.Fatalf("decode explicit values: %v", err)
	}
	if decoded.ChartName == nil || *decoded.ChartName != "" || decoded.WorkloadGatewayIndex == nil || *decoded.WorkloadGatewayIndex != 0 || decoded.VXLANStartupProbeEnabled == nil || !*decoded.VXLANStartupProbeEnabled {
		t.Fatalf("YAML decode lost explicit values: %#v", decoded)
	}
	if _, err := ResolveGeneration(decoded); err == nil {
		t.Fatal("ResolveGeneration() accepted explicit empty/zero values")
	}

	var jsonDecoded GenerationOptions
	if err := json.Unmarshal([]byte(`{"chartName":"","workloadGatewayIndex":0,"vxlanStartupProbeEnabled":true}`), &jsonDecoded); err != nil {
		t.Fatalf("decode JSON explicit values: %v", err)
	}
	if jsonDecoded.ChartName == nil || *jsonDecoded.ChartName != "" || jsonDecoded.WorkloadGatewayIndex == nil || *jsonDecoded.WorkloadGatewayIndex != 0 || jsonDecoded.VXLANStartupProbeEnabled == nil || !*jsonDecoded.VXLANStartupProbeEnabled {
		t.Fatalf("JSON decode lost explicit values: %#v", jsonDecoded)
	}
	if _, err := ResolveGeneration(jsonDecoded); err == nil {
		t.Fatal("ResolveGeneration() accepted JSON explicit empty/zero values")
	}
}

func TestValidateGenerationChecksWorkloadPoolRelationship(t *testing.T) {
	definition, err := LoadDefinition()
	if err != nil {
		t.Fatal(err)
	}
	profile := definition.Defaults.Networking
	generation := definition.Defaults.Generation
	profile.WorkloadCIDR = "10.0.124.0/22"
	if err := ValidateGeneration(profile, generation); err == nil {
		t.Fatal("ValidateGeneration() accepted overlapping loopback/workload pools")
	}
	profile = definition.Defaults.Networking
	generation.WorkloadGatewayIndex = 7
	if err := ValidateGeneration(profile, generation); err == nil {
		t.Fatal("ValidateGeneration() accepted a gateway outside a /29 allocation")
	}
}

func TestLoadDefinitionStrictlyValidatesEmbeddedProfile(t *testing.T) {
	base := string(dpfassets.DPFHBNBGPProfile())
	replace := func(old, new string) []byte {
		t.Helper()
		if !strings.Contains(base, old) {
			t.Fatalf("test profile does not contain %q", old)
		}
		return []byte(strings.Replace(base, old, new, 1))
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "plugin", data: replace("plugin: dpf", "plugin: network-operator")},
		{name: "name", data: replace("name: hbn-bgp", "name: other")},
		{name: "target", data: replace("target: dpf", "target: host")},
		{name: "empty description", data: replace("description: Flat IPv4 BGP HBN profile for the DPF target.", "description: ''")},
		{name: "missing release", data: replace("release: \"26.8.0\"", "release: ''")},
		{name: "missing namespace", data: replace("namespace: dpf-operator-system", "namespace: ''")},
		{name: "missing networking field", data: replace("    workloadPrefix: 29\n", "")},
		{name: "missing generation defaults", data: replace("  generation:\n", "  removed-generation:\n")},
		{name: "missing bootstrap defaults", data: replace("  bootstrap:\n", "  removed-bootstrap:\n")},
		{name: "invalid bootstrap pod CIDR", data: replace("    podCIDR: 10.244.0.0/14", "    podCIDR: 10.244.0.1/14")},
		{name: "invalid bootstrap reset policy", data: replace("    bmcFactoryResetPolicy: Never", "    bmcFactoryResetPolicy: ResetEverything")},
		{name: "unknown root field", data: []byte(base + "unexpected: true\n")},
		{name: "unknown networking field", data: replace("    workloadPrefix: 29\n", "    workloadPrefix: 29\n    unknown: true\n")},
		{name: "second document", data: []byte(base + "---\nplugin: dpf\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeDefinition(test.data); err == nil {
				t.Fatal("decodeDefinition() succeeded; want validation error")
			}
		})
	}
}

func TestLoadDefinitionDoesNotDependOnWorkingDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	if err := os.Chdir(temporary); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	definition, err := LoadDefinition()
	if err != nil {
		t.Fatalf("LoadDefinition() from unrelated directory: %v", err)
	}
	if definition.Name != "hbn-bgp" {
		t.Fatalf("LoadDefinition() name = %q, want hbn-bgp", definition.Name)
	}
}

func TestResolveExplicitValues(t *testing.T) {
	vrf, cidr := "FABRIC", "192.168.8.0/24"
	fabricASN, dpuASN, stride := uint32(64512), uint32(64513), uint32(2)
	uplinkMTU, workloadMTU, workloadPrefix := 9100, 9000, 28
	profile, err := Resolve(Options{
		FabricVRF:      &vrf,
		FabricASN:      &fabricASN,
		DPUASNBase:     &dpuASN,
		DPUASNStride:   &stride,
		UplinkMTU:      &uplinkMTU,
		WorkloadMTU:    &workloadMTU,
		WorkloadCIDR:   &cidr,
		WorkloadPrefix: &workloadPrefix,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := Profile{
		FabricVRF:      vrf,
		FabricASN:      fabricASN,
		DPUASNBase:     dpuASN,
		DPUASNStride:   stride,
		UplinkMTU:      uplinkMTU,
		WorkloadMTU:    workloadMTU,
		WorkloadCIDR:   cidr,
		WorkloadPrefix: workloadPrefix,
	}
	if profile != want {
		t.Fatalf("Resolve() = %#v, want %#v", profile, want)
	}
}

func TestResolveRejectsExplicitInvalidValues(t *testing.T) {
	zero := uint32(0)
	empty := ""
	space := " "
	mtu := 0
	prefix := 0
	base := uint32(65000)
	cidr := "10.0.120.1/22"
	validCIDR := "10.0.120.0/22"
	tooLargeMTU := 9217
	tooSmallUplinkMTU := 1279
	workloadGreaterThanUplink := 9216
	tooLargePrefix := 33
	shortPrefix := 21
	ipv6CIDR := "2001:db8::/64"
	defaultRoute := "0.0.0.0/0"
	uplink := intPointer(9100)

	tests := []struct {
		name    string
		options Options
	}{
		{name: "empty VRF", options: Options{FabricVRF: &empty}},
		{name: "whitespace VRF", options: Options{FabricVRF: &space}},
		{name: "invalid VRF character", options: Options{FabricVRF: stringPointer("bad.vrf")}},
		{name: "explicit zero fabric ASN", options: Options{FabricASN: &zero}},
		{name: "explicit zero DPU ASN", options: Options{DPUASNBase: &zero}},
		{name: "fabric ASN non-private", options: Options{FabricASN: uint32Pointer(65535)}},
		{name: "DPU ASN base non-private", options: Options{DPUASNBase: uint32Pointer(65535)}},
		{name: "DPU ASN base equals fabric", options: Options{FabricASN: &base, DPUASNBase: &base}},
		{name: "explicit zero stride", options: Options{DPUASNStride: &zero}},
		{name: "explicit zero uplink MTU", options: Options{UplinkMTU: &mtu}},
		{name: "uplink MTU below IPv6 link minimum", options: Options{UplinkMTU: &tooSmallUplinkMTU}},
		{name: "uplink MTU too large", options: Options{UplinkMTU: &tooLargeMTU}},
		{name: "explicit zero workload MTU", options: Options{WorkloadMTU: &mtu}},
		{name: "workload MTU above uplink", options: Options{UplinkMTU: uplink, WorkloadMTU: &workloadGreaterThanUplink}},
		{name: "explicit empty CIDR", options: Options{WorkloadCIDR: &empty}},
		{name: "noncanonical CIDR", options: Options{WorkloadCIDR: &cidr}},
		{name: "IPv6 CIDR", options: Options{WorkloadCIDR: &ipv6CIDR}},
		{name: "default route CIDR", options: Options{WorkloadCIDR: &defaultRoute}},
		{name: "prefix shorter than CIDR", options: Options{WorkloadCIDR: &validCIDR, WorkloadPrefix: &shortPrefix}},
		{name: "prefix above 32", options: Options{WorkloadPrefix: &tooLargePrefix}},
		{name: "explicit zero prefix", options: Options{WorkloadCIDR: &validCIDR, WorkloadPrefix: &prefix}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.options); err == nil {
				t.Fatal("Resolve() succeeded; want validation error")
			}
		})
	}

	var decoded Options
	if err := yaml.Unmarshal([]byte("fabricVRF: ''\nfabricASN: 0\n"), &decoded); err != nil {
		t.Fatalf("decode explicit zero/empty values: %v", err)
	}
	if decoded.FabricVRF == nil || *decoded.FabricVRF != "" || decoded.FabricASN == nil || *decoded.FabricASN != 0 {
		t.Fatalf("yaml decode lost explicit values: %#v", decoded)
	}
	if _, err := Resolve(decoded); err == nil {
		t.Fatal("Resolve() accepted explicit YAML zero/empty values")
	}

	var jsonDecoded Options
	if err := json.Unmarshal([]byte(`{"fabricVRF":"","fabricASN":0}`), &jsonDecoded); err != nil {
		t.Fatalf("decode JSON explicit zero/empty values: %v", err)
	}
	if jsonDecoded.FabricVRF == nil || *jsonDecoded.FabricVRF != "" || jsonDecoded.FabricASN == nil || *jsonDecoded.FabricASN != 0 {
		t.Fatalf("JSON decode lost explicit values: %#v", jsonDecoded)
	}
	if _, err := Resolve(jsonDecoded); err == nil {
		t.Fatal("Resolve() accepted explicit JSON zero/empty values")
	}
}

func TestResolveDoesNotRetainOptionPointersOrMutateDefaults(t *testing.T) {
	vrf := "tenant"
	options := Options{FabricVRF: &vrf}
	profile, err := Resolve(options)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	vrf = "changed"
	if profile.FabricVRF != "tenant" {
		t.Fatalf("resolved profile changed with option pointer: %q", profile.FabricVRF)
	}
	profile.FabricVRF = "mutated"
	second, err := Resolve(Options{})
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	if second.FabricVRF != "default" {
		t.Fatalf("mutating first result changed defaults: %#v", second)
	}
}

func TestResolveAllowsMinimumWorkloadMTUWhenUplinkCanCarryIPv6(t *testing.T) {
	uplinkMTU, workloadMTU := 1280, 576
	profile, err := Resolve(Options{UplinkMTU: &uplinkMTU, WorkloadMTU: &workloadMTU})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if profile.UplinkMTU != uplinkMTU || profile.WorkloadMTU != workloadMTU {
		t.Fatalf("resolved MTUs = %d/%d, want %d/%d", profile.UplinkMTU, profile.WorkloadMTU, uplinkMTU, workloadMTU)
	}
}

func TestAllocateASNsStableBySerial(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	first, err := profile.AllocateASNs([]string{"serial-c", "serial-a", "serial-b"})
	if err != nil {
		t.Fatalf("AllocateASNs() error = %v", err)
	}
	second, err := profile.AllocateASNs([]string{"serial-b", "serial-c", "serial-a"})
	if err != nil {
		t.Fatalf("AllocateASNs() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ASN allocations depend on input order: %#v != %#v", first, second)
	}
	want := map[string]uint32{"serial-a": 65101, "serial-b": 65201, "serial-c": 65301}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("ASN allocations = %#v, want %#v", first, want)
	}
}

func TestAllocateASNRejectsInvalidSerialsAndBounds(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	tests := []struct {
		name    string
		serials []string
	}{
		{name: "empty", serials: []string{""}},
		{name: "blank", serials: []string{"  "}},
		{name: "padded", serials: []string{" serial-a "}},
		{name: "duplicate", serials: []string{"serial-a", "serial-a"}},
		{name: "too many", serials: makeSerials(maxDevices + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := profile.AllocateASNs(test.serials); err == nil {
				t.Fatal("AllocateASNs() succeeded; want error")
			}
		})
	}
}

func TestAllocateASNRejectsCollisionAndNonPrivateOverflow(t *testing.T) {
	fabricASN, base, stride := uint32(65201), uint32(65101), uint32(100)
	profile, err := Resolve(Options{FabricASN: &fabricASN, DPUASNBase: &base, DPUASNStride: &stride})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if _, err := profile.AllocateASNs([]string{"serial-a", "serial-b"}); err == nil {
		t.Fatal("AllocateASNs() accepted ASN colliding with fabricASN")
	}

	fabricASN, base, stride = 64512, 65534, 1
	profile, err = Resolve(Options{FabricASN: &fabricASN, DPUASNBase: &base, DPUASNStride: &stride})
	if err != nil {
		t.Fatalf("Resolve() at private-range edge error = %v", err)
	}
	if _, err := profile.AllocateASNs([]string{"serial-a", "serial-b"}); err == nil {
		t.Fatal("AllocateASNs() allocated ASN outside private range")
	}

	fabricASN, base, stride = 64512, 4294967294, 1
	profile, err = Resolve(Options{FabricASN: &fabricASN, DPUASNBase: &base, DPUASNStride: &stride})
	if err != nil {
		t.Fatalf("Resolve() at uint32 edge error = %v", err)
	}
	if _, err := profile.AllocateASNs([]string{"serial-a", "serial-b"}); err == nil {
		t.Fatal("AllocateASNs() accepted uint32 ASN overflow")
	}
}

func TestAllocateASNRevalidatesMutableProfile(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	profile.UplinkMTU = 576
	if _, err := profile.AllocateASNs([]string{"serial-a"}); err == nil {
		t.Fatal("AllocateASNs() accepted a mutated invalid profile")
	}
}

func makeSerials(count int) []string {
	serials := make([]string, count)
	for i := range serials {
		serials[i] = fmt.Sprintf("serial-%03d", i)
	}
	return serials
}

func stringPointer(value string) *string { return &value }

func uint32Pointer(value uint32) *uint32 { return &value }

func intPointer(value int) *int { return &value }
