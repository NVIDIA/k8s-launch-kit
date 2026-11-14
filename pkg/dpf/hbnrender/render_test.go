// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbnrender

import (
	"bytes"
	"embed"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"gopkg.in/yaml.v3"
	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"
	sigsyaml "sigs.k8s.io/yaml"
)

func testInputs(t *testing.T) Inputs {
	t.Helper()
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	asn, err := profile.AllocateASNs([]string{"SERIAL-A"})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := hbn.ResolveGeneration(hbn.GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return Inputs{
		Namespace: "dpf-operator-system", Profile: profile, Generation: generation,
		Software:           config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: digest('a')}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: digest('b')}}},
		Devices:            []Device{{Serial: "SERIAL-A", PSID: "MT_0000001774", SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-a"}, ASN: asn["SERIAL-A"]}},
		DPUClusterSelector: map[string]string{"inventory.example/tenant": "tenant-a"},
		FlavorSpec:         map[string]any{"dpuMode": "dpu"},
		HBNResources:       map[string]any{"memory": "6Gi", "nvidia.com/bf_sf": int64(4)},
	}
}

func digest(ch byte) string { return strings.Repeat(string(ch), 64) }

func TestWorkloadPoolCapacityCoversEverySelectedDPU(t *testing.T) {
	input := testInputs(t)
	// Isolate workload capacity from the independently validated ASN range.
	input.Profile.DPUASNStride = 1
	input.Devices = nil
	for i := range 129 {
		serial := fmt.Sprintf("DPU-%03d", i)
		input.Devices = append(input.Devices, Device{Serial: serial, PSID: "MT_0000001774", ASN: input.Profile.DPUASNBase + uint32(i), SelectorLabels: map[string]string{"inventory.example/serial": serial}})
	}
	files, err := Render(input)
	if err == nil || !strings.Contains(err.Error(), "workloadCIDR") || files != nil {
		t.Fatalf("129 DPUs cannot fit 128 workload blocks: files=%d error=%v", len(files), err)
	}
	input.Devices = input.Devices[:128]
	if _, err := Render(input); err != nil {
		t.Fatalf("exact workload capacity rejected: %v", err)
	}
}

func TestRenderProducesPinnedFlatWorkloadGraph(t *testing.T) {
	input := testInputs(t)
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 10 {
		t.Fatalf("got %d resources, want 10", len(files))
	}
	objects := make([]map[string]any, len(files))
	for i, f := range files {
		if len(f.Data) == 0 || f.Path == "" {
			t.Fatalf("file %d has no path/data", i)
		}
		if err := yaml.Unmarshal(f.Data, &objects[i]); err != nil {
			t.Fatalf("decode %s: %v", f.Path, err)
		}
		if got := objects[i]["kind"]; got == "" {
			t.Fatalf("%s has no kind", f.Path)
		}
	}
	if got := objects[0]["kind"]; got != "BlueFieldSoftware" {
		t.Fatalf("first resource kind=%v", got)
	}
	if got := objects[9]["kind"]; got != "DPUDeployment" {
		t.Fatalf("last resource kind=%v", got)
	}
	if got := nestedString(t, objects[0], "spec", "osIso"); got != input.Software.OSISO.URL {
		t.Fatalf("OS ISO URL=%q", got)
	}
	if got := nestedString(t, objects[7], "spec", "helmChart", "values", "image", "tag"); got != input.Generation.ImageTag+"@"+input.Generation.ImageDigest {
		t.Fatalf("image tag=%q", got)
	}
	if got := nestedBool(t, objects[7], "spec", "helmChart", "values", "hbnSidecar", "vxlanStartupProbe", "enabled"); got {
		t.Fatal("vxlan startup probe must be disabled for flat profile")
	}
	for i, iface := range []string{"p0_if", "p1_if", "pf0hpf_if"} {
		if got := nestedString(t, objects[8], "spec", "interfaces", fmt.Sprint(i), "name"); got != iface {
			t.Fatalf("HBN service interface %d=%q, want %q", i, got, iface)
		}
	}
	for _, index := range []int{4, 5, 6} {
		if got := nestedString(t, objects[index], "spec", "dpuClusterSelector", "matchLabels", "inventory.example/tenant"); got != "tenant-a" {
			t.Fatalf("interface %s tenant selector=%q", objects[index]["metadata"], got)
		}
		if got := nestedString(t, objects[index], "spec", "template", "spec", "nodeSelector", "matchExpressions", "0", "key"); got != "kubernetes.io/hostname" {
			t.Fatalf("interface node selector key=%q", got)
		}
		if got := nestedString(t, objects[index], "spec", "template", "spec", "nodeSelector", "matchExpressions", "0", "operator"); got != "In" {
			t.Fatalf("interface node selector operator=%q", got)
		}
		if got := nestedValue(t, objects[index], "spec", "template", "spec", "nodeSelector", "matchExpressions", "0", "values"); !reflect.DeepEqual(got, []any{"dpu-node-serial-a-serial-a"}) {
			t.Fatalf("interface node selector values=%#v", got)
		}
	}
	startup := nestedString(t, objects[8], "spec", "serviceConfiguration", "helmChart", "values", "configuration", "startupYAMLJ2")
	if bytes.Contains([]byte(startup), []byte(`\n`)) || !bytes.Contains([]byte(startup), []byte("type: unnumbered")) || bytes.Contains([]byte(startup), []byte("evpn:")) || bytes.Contains([]byte(startup), []byte("nve:")) {
		t.Fatalf("startup config is malformed or not flat: %s", startup)
	}
	var rules []struct {
		HostnamePattern string         `yaml:"hostnamePattern"`
		Values          map[string]any `yaml:"values"`
	}
	perDPU := nestedString(t, objects[8], "spec", "serviceConfiguration", "helmChart", "values", "configuration", "perDPUValuesYAML")
	if err := yaml.Unmarshal([]byte(perDPU), &rules); err != nil {
		t.Fatalf("decode per-DPU values: %v", err)
	}
	if len(rules) != 1 || rules[0].HostnamePattern != "dpu-node-serial-a-serial-a" || strings.ContainsAny(rules[0].HostnamePattern, "*?[]") {
		t.Fatalf("per-DPU hostname is not exact: %#v", rules)
	}
	if got := nestedString(t, objects[9], "spec", "dpus", "dpuSetStrategy", "type"); got != string(input.Generation.DPUSetStrategy) {
		t.Fatalf("strategy=%q", got)
	}
	if got := nestedBool(t, objects[9], "spec", "dpus", "nodeEffect", string(input.Generation.NodeEffect)); !got {
		t.Fatal("explicit resolved node effect missing")
	}
	if got := nestedString(t, objects[9], "spec", "dpus", "dpuSets", "0", "dpuDeviceSelector", "matchLabels", "inventory.example/serial-hash"); got != "serial-a" {
		t.Fatalf("selector=%q", got)
	}
	if got := nestedString(t, objects[9], "spec", "dpus", "blueFieldSoftware"); got != nestedString(t, objects[0], "metadata", "name") {
		t.Fatalf("BlueFieldSoftware reference %q does not match resource name", got)
	}
	if got := nestedString(t, objects[9], "spec", "dpus", "flavor"); got != nestedString(t, objects[1], "metadata", "name") {
		t.Fatalf("DPUFlavor reference %q does not match resource name", got)
	}
	for _, iface := range []string{"p0", "p1", "pf0hpf"} {
		if !bytes.Contains(files[9].Data, []byte(iface)) {
			t.Fatalf("deployment chain missing %s", iface)
		}
	}
	if got := nestedString(t, objects[3], "spec", "ipv4Network", "network"); got != input.Profile.WorkloadCIDR {
		t.Fatalf("workload pool=%q", got)
	}
	if got := nestedString(t, objects[3], "spec", "ipv4Network", "prefixSize"); got != "29" {
		t.Fatalf("workload prefix=%q", got)
	}
	if got := nestedString(t, objects[3], "spec", "dpuClusterSelector", "matchLabels", "inventory.example/tenant"); got != "tenant-a" {
		t.Fatalf("tenant selector=%q", got)
	}
	if got := nestedString(t, objects[9], "spec", "dpus", "dpuSets", "0", "dpuClusterSelector", "inventory.example/tenant"); got != "tenant-a" {
		t.Fatalf("deployment tenant selector=%q", got)
	}
	if got := nestedString(t, objects[6], "spec", "template", "spec", "template", "spec", "pf", "pfID"); got != "0" {
		t.Fatalf("PF interface selector=%s", got)
	}
	if got := nestedString(t, objects[3], "spec", "ipv4Network", "gatewayIndex"); got != fmt.Sprint(input.Generation.WorkloadGatewayIndex) {
		t.Fatalf("workload gateway index=%q", got)
	}
	exclusions, ok := nestedValue(t, objects[3], "spec", "ipv4Network", "excludeRanges").([]any)
	if !ok || len(exclusions) != 128 {
		t.Fatalf("workload exclusions = %#v, want 128 singleton host reservations", exclusions)
	}
	for i, want := range []string{"10.0.120.2", "10.0.120.10", "10.0.123.250"} {
		index := []int{0, 1, len(exclusions) - 1}[i]
		rangeMap, ok := exclusions[index].(map[string]any)
		if !ok || rangeMap["startIP"] != want || rangeMap["endIP"] != want {
			t.Fatalf("workload exclusion %d=%#v, want singleton %s", index, exclusions[index], want)
		}
	}
	loopbackNetwork, ok := nestedValue(t, objects[2], "spec", "ipv4Network").(map[string]any)
	if !ok {
		t.Fatal("loopback ipv4Network is not an object")
	}
	if _, ok := loopbackNetwork["excludeRanges"]; ok {
		t.Fatal("loopback pool unexpectedly has host exclusions")
	}
}

func TestRenderDeterministicAndDoesNotMutateInputs(t *testing.T) {
	input := testInputs(t)
	before := append([]string(nil), input.Devices[0].Serial, input.Devices[0].SelectorLabels["inventory.example/serial-hash"], input.Software.PLDM["MT_0000001774"].SHA256)
	first, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatal("render lengths differ")
	}
	for i := range first {
		if first[i].Path != second[i].Path || !bytes.Equal(first[i].Data, second[i].Data) {
			t.Fatalf("render %d differs", i)
		}
	}
	if fmt.Sprint(before) != fmt.Sprint([]string{input.Devices[0].Serial, input.Devices[0].SelectorLabels["inventory.example/serial-hash"], input.Software.PLDM["MT_0000001774"].SHA256}) {
		t.Fatal("render mutated its input")
	}
}

func TestRenderRejectsMissingAndOverlappingIdentity(t *testing.T) {
	input := testInputs(t)
	input.Devices[0].SelectorLabels = nil
	if _, err := Render(input); err == nil {
		t.Fatal("expected missing selector error")
	}
	input = testInputs(t)
	input.Devices[0].SelectorLabels = map[string]string{"invalid key": "serial-a"}
	if _, err := Render(input); err == nil {
		t.Fatal("expected invalid selector label key to be rejected")
	}
	input = testInputs(t)
	input.DPUClusterSelector = nil
	if _, err := Render(input); err == nil {
		t.Fatal("expected missing tenant selector error")
	}
	input = testInputs(t)
	other := input.Devices[0]
	other.Serial = "SERIAL-B"
	other.SelectorLabels = map[string]string{"inventory.example/serial-hash": "serial-b"}
	other.PSID = "MT_0000001774"
	input.Devices = append(input.Devices, other)
	if _, err := Render(input); err == nil {
		t.Fatal("expected selectors that can overlap to be rejected")
	}
	input = testInputs(t)
	input.DPUClusterSelector = map[string]string{"inventory.example/control-plane": ""}
	if _, err := Render(input); err != nil {
		t.Fatalf("valid empty Kubernetes label value was rejected: %v", err)
	}
}

func TestPerDPUASNUsesExactDerivedHostnameAndRejectsCollisions(t *testing.T) {
	input := testInputs(t)
	serials := []string{"SERIAL-A", "SERIAL-B"}
	asns, err := input.Profile.AllocateASNs(serials)
	if err != nil {
		t.Fatal(err)
	}
	input.Devices[0].ASN = asns["SERIAL-A"]
	input.Devices = append(input.Devices, Device{
		Serial:         "SERIAL-B",
		PSID:           "MT_0000001774",
		SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-b"},
		ASN:            asns["SERIAL-B"],
	})
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	var configuration map[string]any
	if err := yaml.Unmarshal(files[8].Data, &configuration); err != nil {
		t.Fatal(err)
	}
	encoded := nestedString(t, configuration, "spec", "serviceConfiguration", "helmChart", "values", "configuration", "perDPUValuesYAML")
	var rules []struct {
		HostnamePattern string         `yaml:"hostnamePattern"`
		Values          map[string]any `yaml:"values"`
	}
	if err := yaml.Unmarshal([]byte(encoded), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d per-DPU rules, want 2: %#v", len(rules), rules)
	}
	want := map[string]uint32{
		"dpu-node-serial-a-serial-a": asns["SERIAL-A"],
		"dpu-node-serial-b-serial-b": asns["SERIAL-B"],
	}
	for _, rule := range rules {
		if strings.ContainsAny(rule.HostnamePattern, "*?[]") {
			t.Fatalf("wildcard hostname rule was emitted: %q", rule.HostnamePattern)
		}
		asn, exists := want[rule.HostnamePattern]
		if !exists {
			t.Fatalf("unexpected hostname rule %q", rule.HostnamePattern)
		}
		if got := fmt.Sprint(rule.Values["bgp_autonomous_system"]); got != fmt.Sprint(asn) {
			t.Fatalf("hostname %q has ASN %s, want %d", rule.HostnamePattern, got, asn)
		}
	}

	// DPF lowercases the serial for both DPUNode and DPUDevice names.
	input = testInputs(t)
	upper := input.Devices[0]
	lower := upper
	lower.Serial = "serial-a"
	lower.SelectorLabels = map[string]string{"inventory.example/serial-hash": "serial-b"}
	input.Devices = append(input.Devices, lower)
	if _, err := Render(input); err == nil {
		t.Fatal("serials mapping to the same DPF hostname were accepted")
	}

	input = testInputs(t)
	input.Devices[0].Serial = "MT_0000001774"
	if _, err := Render(input); err == nil {
		t.Fatal("serial producing an invalid DNS hostname was accepted")
	}
	input = testInputs(t)
	input.Devices[0].Serial = strings.Repeat("a", 30)
	if _, err := Render(input); err == nil {
		t.Fatal("hostname exceeding Kubernetes label-value length was accepted")
	}
}

func TestInterfaceSelectorsRestrictTenantAndSelectedNodes(t *testing.T) {
	input := testInputs(t)
	beforeClusterSelector := cloneStringMap(input.DPUClusterSelector)
	serials := []string{"SERIAL-A", "SERIAL-B"}
	asns, err := input.Profile.AllocateASNs(serials)
	if err != nil {
		t.Fatal(err)
	}
	input.Devices = append(input.Devices, Device{
		Serial: "SERIAL-B", PSID: "MT_0000001774",
		SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-b"}, ASN: asns["SERIAL-B"],
	})
	files, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.DPUClusterSelector, beforeClusterSelector) {
		t.Fatal("render mutated the tenant cluster selector")
	}
	type interfaceSelector struct {
		Spec struct {
			DPUClusterSelector metav1.LabelSelector `yaml:"dpuClusterSelector"`
			Template           struct {
				Spec struct {
					NodeSelector metav1.LabelSelector `yaml:"nodeSelector"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	for _, index := range []int{4, 5, 6} {
		var object interfaceSelector
		if err := sigsyaml.Unmarshal(files[index].Data, &object); err != nil {
			t.Fatalf("decode interface %s: %v", files[index].Path, err)
		}
		clusterSelector, err := metav1.LabelSelectorAsSelector(&object.Spec.DPUClusterSelector)
		if err != nil {
			t.Fatal(err)
		}
		nodeSelector, err := metav1.LabelSelectorAsSelector(&object.Spec.Template.Spec.NodeSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !clusterSelector.Matches(labels.Set{"inventory.example/tenant": "tenant-a", "other": "kept"}) {
			t.Fatalf("%s does not match selected tenant labels", files[index].Path)
		}
		if clusterSelector.Matches(labels.Set{"inventory.example/tenant": "tenant-b"}) {
			t.Fatalf("%s matches excluded tenant", files[index].Path)
		}
		for _, hostname := range []string{"dpu-node-serial-a-serial-a", "dpu-node-serial-b-serial-b"} {
			if !nodeSelector.Matches(labels.Set{"kubernetes.io/hostname": hostname}) {
				t.Fatalf("%s does not match selected node %q", files[index].Path, hostname)
			}
		}
		if nodeSelector.Matches(labels.Set{"kubernetes.io/hostname": "unselected-dpu-node"}) {
			t.Fatalf("%s matches an unselected node", files[index].Path)
		}
		if nodeSelector.Matches(labels.Set{"kubernetes.io/os": "linux"}) {
			t.Fatalf("%s matches a node without the selected hostname label", files[index].Path)
		}
	}

	input.Devices[0], input.Devices[1] = input.Devices[1], input.Devices[0]
	reversed, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{4, 5, 6} {
		if !bytes.Equal(files[index].Data, reversed[index].Data) {
			t.Fatalf("interface selector %s changes with input device order", files[index].Path)
		}
	}
}

func TestServiceTemplateResourceRequirementsMatchHelmAndAreIndependent(t *testing.T) {
	resources := map[string]any{"cpu": "2", "memory": "6Gi", "nvidia.com/bf_sf": int64(4)}
	object := serviceTemplate("dpf-operator-system", hbn.Generation{ChartName: "doca-hbn"}, resources)
	spec := object["spec"].(map[string]any)
	requirements := spec["resourceRequirements"].(map[string]any)
	values := spec["helmChart"].(map[string]any)["values"].(map[string]any)
	chartResources := values["resources"].(map[string]any)
	if !reflect.DeepEqual(requirements, chartResources) {
		t.Fatalf("template resourceRequirements=%v differ from Helm resources=%v", requirements, chartResources)
	}
	chartResources["cpu"] = "4"
	if requirements["cpu"] != "2" {
		t.Fatal("template resourceRequirements shares its map with Helm values")
	}
}

func TestRenderRejectsChartNameTooLongForDPFAPI(t *testing.T) {
	input := testInputs(t)
	input.Generation.ChartName = strings.Repeat("a", 29)
	if _, err := Render(input); err == nil {
		t.Fatal("29-character chart name produced a DPF deploymentServiceName")
	}
}

func TestRenderRejectsInvalidWorkloadPrefixWithoutPanic(t *testing.T) {
	input := testInputs(t)
	input.Profile.WorkloadPrefix = 33
	if _, err := Render(input); err == nil {
		t.Fatal("expected invalid workload prefix error")
	}
}

func TestArtifactDigestParticipatesInImmutableSoftwareIdentity(t *testing.T) {
	input := testInputs(t)
	first, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	var firstSoftware map[string]any
	if err := yaml.Unmarshal(first[0].Data, &firstSoftware); err != nil {
		t.Fatal(err)
	}
	firstName := nestedString(t, firstSoftware, "metadata", "name")
	input.Software.OSISO.SHA256 = digest('c')
	second, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	var secondSoftware map[string]any
	if err := yaml.Unmarshal(second[0].Data, &secondSoftware); err != nil {
		t.Fatal(err)
	}
	if secondName := nestedString(t, secondSoftware, "metadata", "name"); firstName == secondName {
		t.Fatal("changed artifact digest did not change immutable object identity")
	}
}

func TestRenderRejectsUnsafeResolvedGeneration(t *testing.T) {
	input := testInputs(t)
	input.Generation.VXLANStartupProbeEnabled = true
	if _, err := Render(input); err == nil {
		t.Fatal("flat profile must reject the vxlan0 startup probe")
	}
	input = testInputs(t)
	input.Profile.FabricVRF = "tenant"
	files, err := Render(input)
	if err != nil {
		t.Fatalf("switch-local fabric VRF override should be allowed: %v", err)
	}
	var serviceConfig map[string]any
	if err := yaml.Unmarshal(files[8].Data, &serviceConfig); err != nil {
		t.Fatal(err)
	}
	startup := nestedString(t, serviceConfig, "spec", "serviceConfiguration", "helmChart", "values", "configuration", "startupYAMLJ2")
	if !strings.Contains(startup, "    default:\n") || strings.Contains(startup, "    tenant:\n") {
		t.Fatalf("switch-local fabric VRF changed flat HBN's internal default VRF config: %s", startup)
	}
	input = testInputs(t)
	input.Generation.NodeEffect = ""
	if _, err := Render(input); err == nil {
		t.Fatal("renderer must reject absent node effect")
	}
	input = testInputs(t)
	input.Generation.DPUSetStrategy = ""
	if _, err := Render(input); err == nil {
		t.Fatal("renderer must reject absent DPUSet strategy")
	}
	input = testInputs(t)
	input.Generation.ImageDigest = "invalid"
	if _, err := Render(input); err == nil {
		t.Fatal("renderer must reject malformed image digest")
	}
}

func TestRenderRejectsMissingArtifactsAndUnreviewedFlavor(t *testing.T) {
	input := testInputs(t)
	input.Software.PLDM = nil
	if _, err := Render(input); err == nil {
		t.Fatal("expected missing selected PSID artifact error")
	}
	input = testInputs(t)
	input.FlavorSpec = nil
	if _, err := Render(input); err == nil {
		t.Fatal("expected missing DPUFlavor spec error")
	}
}

func nestedString(t *testing.T, root map[string]any, fields ...string) string {
	t.Helper()
	return fmt.Sprint(nestedValue(t, root, fields...))
}

func nestedValue(t *testing.T, root map[string]any, fields ...string) any {
	t.Helper()
	var value any = root
	for _, field := range fields {
		if index, err := fmt.Sscanf(field, "%d", new(int)); err == nil && index == 1 {
			list, ok := value.([]any)
			if !ok {
				t.Fatalf("%v is not list at %q", value, field)
				return nil
			}
			var n int
			_, _ = fmt.Sscanf(field, "%d", &n)
			if n >= len(list) {
				t.Fatalf("index out of range: %s", field)
				return nil
			}
			value = list[n]
			continue
		}
		m, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%v is not map at %q", value, field)
			return nil
		}
		var exists bool
		value, exists = m[field]
		if !exists {
			t.Fatalf("missing field %q in %v", field, m)
			return nil
		}
	}
	return value
}

func nestedBool(t *testing.T, root map[string]any, fields ...string) bool {
	t.Helper()
	value := nestedString(t, root, fields...)
	return value == "true"
}

// These schemas are copied from the verified DPF 26.8.0 chart archive
// (SHA-256 0252d473beaf473e93a09b23a39b7b78fff931c9a801f4e60ef93bface5e28c8).
func TestRenderResourcesValidateAgainstDPF268Schemas(t *testing.T) {
	inputs := testInputs(t)
	files, err := Render(inputs)
	if err != nil {
		t.Fatal(err)
	}
	crds, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		var object map[string]any
		if err := sigsyaml.Unmarshal(file.Data, &object); err != nil {
			t.Fatalf("decode %s: %v", file.Path, err)
		}
		kind, _ := object["kind"].(string)
		schema, ok := crds[kind]
		if !ok {
			t.Fatalf("no exact 26.8.0 schema for %s", kind)
		}
		for _, validationErr := range validation.ValidateCustomResource(field.NewPath("resource"), object, schema) {
			t.Errorf("%s fails selected DPF 26.8.0 CRD validation: %v", file.Path, validationErr)
		}
	}
}

//go:embed testdata/crds/*.yaml
var crdFiles embed.FS

func loadSchemas() (map[string]validation.SchemaCreateValidator, error) {
	entries, err := crdFiles.ReadDir("testdata/crds")
	if err != nil {
		return nil, err
	}
	result := make(map[string]validation.SchemaCreateValidator, len(entries))
	for _, entry := range entries {
		data, err := crdFiles.ReadFile("testdata/crds/" + entry.Name())
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := sigsyaml.Unmarshal(data, &crd); err != nil {
			return nil, fmt.Errorf("decode CRD fixture %s: %w", entry.Name(), err)
		}
		for _, version := range crd.Spec.Versions {
			if version.Name != "v1alpha1" || !version.Served {
				continue
			}
			internalSchema := &apiextensionsinternal.JSONSchemaProps{}
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internalSchema, nil); err != nil {
				return nil, fmt.Errorf("convert schema for %s: %w", crd.Spec.Names.Kind, err)
			}
			validator, _, err := validation.NewSchemaValidator(internalSchema)
			if err != nil {
				return nil, fmt.Errorf("build schema validator for %s: %w", crd.Spec.Names.Kind, err)
			}
			result[crd.Spec.Names.Kind] = validator
			break
		}
	}
	return result, nil
}
