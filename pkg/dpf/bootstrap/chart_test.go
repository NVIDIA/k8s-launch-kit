// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/postrender"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeutilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

//go:embed testdata/operator/dpf-operator-v26.8.0.tgz
var operatorChart268 []byte

func TestRenderOperatorPinnedChart(t *testing.T) {
	inputs := validOperatorRenderInputs()
	inputs.Values = map[string]any{"controllerManager": map[string]any{"replicas": 2}}
	valuesBefore := map[string]any{"controllerManager": map[string]any{"replicas": 2}}

	first, err := RenderOperator(operatorChart268, inputs)
	if err != nil {
		t.Fatalf("RenderOperator() error = %v", err)
	}
	second, err := RenderOperator(operatorChart268, inputs)
	if err != nil {
		t.Fatalf("second RenderOperator() error = %v", err)
	}
	if !bytes.Equal(first.Manifest, second.Manifest) || !bytes.Equal(first.HelmValues, second.HelmValues) {
		t.Fatal("RenderOperator() output is not deterministic")
	}
	if !reflect.DeepEqual(inputs.Values, valuesBefore) {
		t.Fatal("RenderOperator() mutated its inputs")
	}
	if first.PostRenderer == nil {
		t.Fatal("RenderOperator() PostRenderer is nil")
	}
	if got, want := string(first.HelmValues), "controllerManager:\n    replicas: 2\n"; got != want {
		t.Fatalf("HelmValues = %q, want %q", got, want)
	}
	if !strings.Contains(string(first.Manifest), "namespace: tenant-operator") {
		t.Fatal("rendered manifest does not use selected operator namespace")
	}
	objects := decodeRenderedObjects(t, first.Manifest)
	var foundDeployment bool
	for _, object := range objects {
		if object.GetKind() != "Deployment" || object.GetName() != operatorDeploymentName {
			continue
		}
		foundDeployment = true
		if got := object.GetNamespace(); got != "tenant-operator" {
			t.Errorf("operator deployment namespace = %q", got)
		}
		replicas, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "replicas")
		if err != nil || !found || replicas != float64(2) {
			t.Errorf("deployment replicas = %#v, found %t, error %v", replicas, found, err)
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			t.Fatalf("operator containers missing: found=%t err=%v", found, err)
		}
		manager := mapValueByName(t, containers, operatorContainerName)
		args, ok := manager["args"].([]any)
		if !ok || !containsStringAny(args, "--config-namespace=tenant-operator") || !containsStringAny(args, "--config-name=tenant-dpf-config") {
			t.Errorf("operator args do not bind selected config: %#v", manager["args"])
		}
		if !containsStringAny(args, "--leader-elect=true") {
			t.Errorf("operator args lost chart leader election flag: %#v", args)
		}
	}
	if !foundDeployment {
		t.Fatal("rendered chart has no operator Deployment")
	}
}

func TestRenderOperatorRejectsInvalidInputsAndArchive(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OperatorRenderInputs)
		chart  []byte
	}{
		{name: "missing release name", mutate: func(in *OperatorRenderInputs) { in.ReleaseName = "" }},
		{name: "unsupported Kubernetes release", mutate: func(in *OperatorRenderInputs) { in.KubernetesVersion = "1.37.0" }},
		{name: "malformed Kubernetes version", mutate: func(in *OperatorRenderInputs) { in.KubernetesVersion = "latest" }},
		{name: "unsupported DPF cohort", mutate: func(in *OperatorRenderInputs) { in.ReleaseVersion = "26.4.0" }},
		{name: "bad chart digest", chart: []byte("not the pinned chart")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs := validOperatorRenderInputs()
			if test.mutate != nil {
				test.mutate(&inputs)
			}
			chart := operatorChart268
			if test.chart != nil {
				chart = test.chart
			}
			if _, err := RenderOperator(chart, inputs); err == nil {
				t.Fatal("RenderOperator() succeeded, want error")
			}
		})
	}
}

func TestPinnedOperatorChartDoesNotDependOnHelmCapabilities(t *testing.T) {
	archive, err := gzip.NewReader(bytes.NewReader(operatorChart268))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("read pinned operator chart archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read chart file %q: %v", header.Name, err)
		}
		if bytes.Contains(data, []byte(".Capabilities")) {
			t.Fatalf("pinned chart file %q consumes Helm capabilities; render inputs must be reviewed before removing this assertion", header.Name)
		}
	}
}

func TestRenderOperatorUsesSelectedReleaseManagerImage(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository string
		tag        string
		wantErr    bool
	}{
		{name: "matching explicit image", repository: "nvcr.io/nvidia/doca/dpf-system", tag: "v26.8.0"},
		{name: "conflicting repository", repository: "registry.example/dpf-system", tag: "v26.8.0", wantErr: true},
		{name: "conflicting tag", repository: "nvcr.io/nvidia/doca/dpf-system", tag: "v26.8.1", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputs := validOperatorRenderInputs()
			inputs.Values = map[string]any{"controllerManager": map[string]any{"image": map[string]any{"repository": test.repository, "tag": test.tag}}}
			before, _, err := canonicalHelmValues(inputs.Values)
			if err != nil {
				t.Fatalf("canonicalHelmValues() before render: %v", err)
			}

			result, err := RenderOperator(operatorChart268, inputs)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "conflicts with the selected release catalog") {
					t.Fatalf("RenderOperator() error = %v, want selected release image conflict", err)
				}
				if !reflect.DeepEqual(result, OperatorRenderResult{}) {
					t.Fatalf("RenderOperator() returned partial result on error: %#v", result)
				}
			} else {
				if err != nil {
					t.Fatalf("RenderOperator() error = %v", err)
				}
				var managerImage string
				for _, object := range decodeRenderedObjects(t, result.Manifest) {
					if object.GetKind() != "Deployment" || object.GetName() != operatorDeploymentName {
						continue
					}
					containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
					if err != nil || !found {
						t.Fatalf("manager containers found=%t err=%v", found, err)
					}
					managerImage = mapValueByName(t, containers, operatorContainerName)["image"].(string)
				}
				if managerImage != "nvcr.io/nvidia/doca/dpf-system:v26.8.0" {
					t.Fatalf("manager image=%q, want selected catalog image", managerImage)
				}
			}
			after, _, err := canonicalHelmValues(inputs.Values)
			if err != nil {
				t.Fatalf("canonicalHelmValues() after render: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("RenderOperator() mutated explicit values")
			}
		})
	}
}

func TestOrderedOperatorManifestsRejectsHooks(t *testing.T) {
	_, err := orderedOperatorManifests(map[string]string{
		"templates/hook.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hook\n  annotations:\n    helm.sh/hook: pre-install\n",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported Helm hook") {
		t.Fatalf("orderedOperatorManifests() error = %v, want unsupported hook", err)
	}
}

func validOperatorRenderInputs() OperatorRenderInputs {
	return OperatorRenderInputs{
		ReleaseVersion:    "26.8.0",
		ReleaseName:       "dpf",
		KubernetesVersion: "1.35.4",
		Values:            map[string]any{},
		Bootstrap:         Inputs{OperatorNamespace: "tenant-operator", OperatorConfigName: "tenant-dpf-config"},
	}
}

func decodeRenderedObjects(t *testing.T, manifest []byte) []*unstructured.Unstructured {
	t.Helper()
	reader := kubeutilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	var objects []*unstructured.Unstructured
	for {
		document, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return objects
			}
			t.Fatalf("read rendered manifest: %v", err)
		}
		if len(bytes.TrimSpace(document)) == 0 {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(document, &object.Object); err != nil {
			t.Fatalf("decode rendered object: %v", err)
		}
		objects = append(objects, object)
	}
}

func mapValueByName(t *testing.T, values []any, name string) map[string]any {
	t.Helper()
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if object["name"] == name {
			return object
		}
	}
	t.Fatalf("container %q not found", name)
	return nil
}

func containsStringAny(values []any, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var _ postrender.PostRenderer = operatorPostRenderer{}
