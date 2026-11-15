// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"bufio"
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeutilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// Chart fixtures are copied from verified DPF 26.8.0 chart archive SHA-256
// 0252d473beaf473e93a09b23a39b7b78fff931c9a801f4e60ef93bface5e28c8.
func renderPinnedOperatorChart(t *testing.T, namespace string) []byte {
	t.Helper()
	chart, err := loader.Load("testdata/operator")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"controllerManager": map[string]any{"featureGates": map[string]any{"ConfigPortsOverHighSpeed": true}}}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "dpf-operator", Namespace: namespace, IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(manifests))
	for path := range manifests {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var output bytes.Buffer
	for i, path := range paths {
		if i != 0 {
			output.WriteString("---\n")
		}
		output.WriteString(manifests[path])
	}
	return output.Bytes()
}

func TestOperatorPostRendererBindsSelectedConfigAndPreservesChartContent(t *testing.T) {
	inputs := Inputs{OperatorNamespace: "custom-dpf-system", OperatorConfigName: "custom-config"}
	post, err := NewOperatorPostRenderer(inputs)
	if err != nil {
		t.Fatal(err)
	}
	chart := renderPinnedOperatorChart(t, inputs.OperatorNamespace)
	original := append([]byte(nil), chart...)
	chart = addTestSidecarAndUnrelatedDoc(t, chart)
	beforeRun := append([]byte(nil), chart...)
	modified, err := post.Run(bytes.NewBuffer(chart))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chart, beforeRun) {
		t.Fatal("postrenderer mutated input bytes")
	}
	if !bytes.Equal(original, renderPinnedOperatorChart(t, inputs.OperatorNamespace)) {
		t.Fatal("Helm render fixture unexpectedly changed")
	}

	docs := decodeOperatorDocuments(t, modified.Bytes())
	if len(docs) != 2 {
		t.Fatalf("got %d output docs, want Deployment and unrelated ConfigMap", len(docs))
	}
	deployment := docs[0]
	if deployment.GetName() != operatorDeploymentName || deployment.GetNamespace() != inputs.OperatorNamespace {
		t.Fatalf("operator identity changed: %s/%s", deployment.GetNamespace(), deployment.GetName())
	}
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 2 {
		t.Fatalf("containers=%v err=%v", containers, err)
	}
	manager := containers[0].(map[string]any)
	args := mustStringSlice(t, manager["args"])
	for _, expected := range []string{"--leader-elect=true", "--feature-gates=ConfigPortsOverHighSpeed=true", "--config-namespace=custom-dpf-system", "--config-name=custom-config"} {
		if !containsString(args, expected) {
			t.Errorf("operator args %v omit %q", args, expected)
		}
	}
	if countFlag(args, operatorConfigNSFlag) != 1 || countFlag(args, operatorConfigNameFlag) != 1 {
		t.Fatalf("config flags were duplicated: %v", args)
	}
	sidecar := containers[1].(map[string]any)
	if got := mustStringSlice(t, sidecar["args"]); strings.Join(got, ",") != "--keep=this" {
		t.Fatalf("non-operator container args changed: %v", got)
	}
	if docs[1].GetKind() != "ConfigMap" || docs[1].GetName() != "unrelated" {
		t.Fatalf("unrelated document changed: %s/%s", docs[1].GetKind(), docs[1].GetName())
	}
	data, found, err := unstructured.NestedStringMap(docs[1].Object, "data")
	if err != nil || !found || data["keep"] != "unchanged" {
		t.Fatalf("unrelated ConfigMap changed: %#v (%v)", data, err)
	}
}

func TestOperatorPostRendererDeduplicatesMatchingConfigFlags(t *testing.T) {
	inputs := Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "dpfoperatorconfig"}
	post, err := NewOperatorPostRenderer(inputs)
	if err != nil {
		t.Fatal(err)
	}
	stream := mutateDeployment(t, renderPinnedOperatorChart(t, inputs.OperatorNamespace), func(t *testing.T, obj *unstructured.Unstructured) {
		mutateManager(t, obj, func(container map[string]any) {
			args := container["args"].([]any)
			container["args"] = append(args, "--config-name", "dpfoperatorconfig", "--config-namespace=dpf-system")
		})
	})
	modified, err := post.Run(bytes.NewBuffer(stream))
	if err != nil {
		t.Fatal(err)
	}
	deployment := decodeOperatorDocuments(t, modified.Bytes())[0]
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	args := mustStringSlice(t, containers[0].(map[string]any)["args"])
	if countFlag(args, operatorConfigNameFlag) != 1 || countFlag(args, operatorConfigNSFlag) != 1 {
		t.Fatalf("expected one config binding flag each, got %v", args)
	}
	if !containsString(args, "--leader-elect=true") || !containsString(args, "--feature-gates=ConfigPortsOverHighSpeed=true") {
		t.Fatalf("unrelated chart flags were lost: %v", args)
	}
}

func TestOperatorPostRendererRejectsUnsafeChartShapes(t *testing.T) {
	inputs := Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "dpfoperatorconfig"}
	post, err := NewOperatorPostRenderer(inputs)
	if err != nil {
		t.Fatal(err)
	}
	valid := renderPinnedOperatorChart(t, inputs.OperatorNamespace)
	cases := []struct {
		name   string
		mutate func(*testing.T, *unstructured.Unstructured)
	}{
		{name: "wrong deployment name", mutate: func(t *testing.T, obj *unstructured.Unstructured) { obj.SetName("renamed-operator") }},
		{name: "wrong namespace", mutate: func(t *testing.T, obj *unstructured.Unstructured) { obj.SetNamespace("other-system") }},
		{name: "missing manager container", mutate: func(t *testing.T, obj *unstructured.Unstructured) { setContainerName(t, obj, "manager", "other") }},
		{name: "wrong operator command", mutate: func(t *testing.T, obj *unstructured.Unstructured) {
			setContainerCommand(t, obj, "manager", []any{"/different"})
		}},
		{name: "duplicate manager containers", mutate: func(t *testing.T, obj *unstructured.Unstructured) { duplicateManager(t, obj) }},
		{name: "conflicting config name", mutate: func(t *testing.T, obj *unstructured.Unstructured) {
			appendManagerArg(t, obj, "--config-name=wrong-name")
		}},
		{name: "conflicting config namespace", mutate: func(t *testing.T, obj *unstructured.Unstructured) {
			appendManagerArg(t, obj, "--config-namespace=wrong-system")
		}},
		{name: "missing separated config flag value", mutate: func(t *testing.T, obj *unstructured.Unstructured) { appendManagerArg(t, obj, "--config-name") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := mutateDeployment(t, valid, tc.mutate)
			if _, err := post.Run(bytes.NewBuffer(stream)); err == nil {
				t.Fatal("expected renderer to reject chart shape")
			}
		})
	}
	if _, err := post.Run(bytes.NewBufferString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n")); err == nil {
		t.Fatal("expected missing operator Deployment error")
	}
	if _, err := post.Run(nil); err == nil {
		t.Fatal("expected nil Helm buffer error")
	}
	if _, err := NewOperatorPostRenderer(Inputs{OperatorNamespace: "bad namespace", OperatorConfigName: "config"}); err == nil {
		t.Fatal("expected invalid namespace error")
	}
	if _, err := NewOperatorPostRenderer(Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "invalid.config"}); err == nil {
		t.Fatal("expected invalid DPFOperatorConfig name error")
	}
}

func TestOperatorPostRendererRejectsAmbiguousDeployments(t *testing.T) {
	inputs := Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "dpfoperatorconfig"}
	post, err := NewOperatorPostRenderer(inputs)
	if err != nil {
		t.Fatal(err)
	}
	valid := renderPinnedOperatorChart(t, inputs.OperatorNamespace)
	if _, err := post.Run(bytes.NewBuffer(append(append([]byte(nil), valid...), append([]byte("---\n"), valid...)...))); err == nil {
		t.Fatal("expected duplicate DPF operator Deployments to fail")
	}
}

func addTestSidecarAndUnrelatedDoc(t *testing.T, stream []byte) []byte {
	t.Helper()
	deployment := decodeOperatorDocuments(t, stream)[0]
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	containers = append(containers, map[string]any{"name": "sidecar", "image": "example/sidecar:1", "args": []any{"--keep=this"}})
	if err := unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	depYAML, err := sigsyaml.Marshal(deployment.Object)
	if err != nil {
		t.Fatal(err)
	}
	otherYAML := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n  namespace: dpf-system\ndata:\n  keep: unchanged\n")
	return bytes.Join([][]byte{depYAML, otherYAML}, []byte("---\n"))
}

func mutateDeployment(t *testing.T, stream []byte, mutate func(*testing.T, *unstructured.Unstructured)) []byte {
	t.Helper()
	objects := decodeOperatorDocuments(t, stream)
	mutate(t, objects[0])
	data, err := sigsyaml.Marshal(objects[0].Object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeOperatorDocuments(t *testing.T, stream []byte) []*unstructured.Unstructured {
	t.Helper()
	reader := kubeutilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(stream)))
	var result []*unstructured.Unstructured
	for {
		raw, err := reader.Read()
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(raw, &object.Object); err != nil {
			t.Fatal(err)
		}
		result = append(result, object)
	}
}

func setContainerName(t *testing.T, obj *unstructured.Unstructured, old, next string) {
	t.Helper()
	mutateManager(t, obj, func(container map[string]any) {
		if container["name"] != old {
			t.Fatalf("unexpected name %v", container["name"])
		}
		container["name"] = next
	})
}

func setContainerCommand(t *testing.T, obj *unstructured.Unstructured, name string, command []any) {
	t.Helper()
	mutateManager(t, obj, func(container map[string]any) {
		if container["name"] != name {
			t.Fatalf("unexpected container %v", container["name"])
		}
		container["command"] = command
	})
}

func appendManagerArg(t *testing.T, obj *unstructured.Unstructured, arg string) {
	t.Helper()
	mutateManager(t, obj, func(container map[string]any) {
		args := container["args"].([]any)
		container["args"] = append(args, arg)
	})
}

func duplicateManager(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	containers = append(containers, map[string]any{"name": "manager", "command": []any{"/operator"}})
	if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
}

func mutateManager(t *testing.T, obj *unstructured.Unstructured, mutate func(map[string]any)) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, raw := range containers {
		container := raw.(map[string]any)
		if container["name"] == "manager" {
			mutate(container)
			containers[i] = container
			found = true
		}
	}
	if !found {
		t.Fatal("manager not found")
	}
	if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
}

func mustStringSlice(t *testing.T, value any) []string {
	t.Helper()
	result, ok := stringSlice(value)
	if !ok {
		t.Fatalf("not a string slice: %#v", value)
	}
	return result
}
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func countFlag(args []string, flag string) int {
	count := 0
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			count++
		}
	}
	return count
}

func TestOperatorPostRendererPreservesClusterScopedRule(t *testing.T) {
	in := Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "dpfoperatorconfig"}
	renderer, err := NewOperatorPostRenderer(in)
	if err != nil {
		t.Fatal(err)
	}
	deployment := renderPinnedOperatorChart(t, in.OperatorNamespace)
	for _, namespace := range []string{"dpf-system", ""} {
		rule := []byte("apiVersion: nfd.k8s-sigs.io/v1alpha1\nkind: NodeFeatureRule\nmetadata:\n  name: dpu-detection-rule\n  namespace: " + namespace + "\nspec:\n  rules: [{name: keep, labels: {dpu-enabled: 'true'}}]\n")
		input := append(append(append([]byte{}, deployment...), []byte("\n---\n")...), rule...)
		result, err := renderer.Run(bytes.NewBuffer(input))
		if err != nil {
			t.Fatal(err)
		}
		docs := decodeOperatorDocuments(t, result.Bytes())
		if len(docs) != 2 || docs[1].GetNamespace() != "" || docs[1].GetName() != "dpu-detection-rule" {
			t.Fatal("cluster-scoped rule lost or still namespaced")
		}
		labels, _, err := unstructured.NestedSlice(docs[1].Object, "spec", "rules")
		if err != nil || len(labels) != 1 {
			t.Fatal("rule content lost")
		}
		again, err := renderer.Run(bytes.NewBuffer(result.Bytes()))
		if err != nil || !bytes.Equal(again.Bytes(), result.Bytes()) {
			t.Fatal("postrenderer is not idempotent", err)
		}
	}
	foreign := append(append([]byte{}, deployment...), []byte("\n---\napiVersion: nfd.k8s-sigs.io/v1alpha1\nkind: NodeFeatureRule\nmetadata: {name: dpu-detection-rule, namespace: other}\n")...)
	result, err := renderer.Run(bytes.NewBuffer(foreign))
	if err == nil || result != nil {
		t.Fatal("accepted a foreign namespace")
	}
}
