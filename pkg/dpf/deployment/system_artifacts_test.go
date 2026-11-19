// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSystemArtifactSourceVerification(t *testing.T) {
	testArchive := []byte("test archive")
	source := releases.SystemNetworking{ChartDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(testArchive))}
	for _, test := range []struct {
		name    string
		archive []byte
		wantErr string
	}{
		{name: "invalid empty size", wantErr: "size"},
		{name: "invalid large size", archive: make([]byte, maximumSystemChartBytes+1), wantErr: "size"},
		{name: "digest mismatch", archive: []byte("changed"), wantErr: "digest"},
		{name: "verified bytes", archive: testArchive},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifySystemArchive(test.archive, source)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected %s error, got %v", test.wantErr, err)
			}
		})
	}
	if _, err := projectSystemArtifacts(testArchive, generation.ResolvedIntent{Release: "26.4.0", UseCase: releases.UseCaseHBNBGP}); err == nil || !strings.Contains(err.Error(), "release or profile") {
		t.Fatalf("unsupported release should fail before archive inspection: %v", err)
	}
	if _, err := projectSystemArtifacts(testArchive, generation.ResolvedIntent{Release: "26.8.0", UseCase: releases.UseCaseHBNBGP}); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("archive digest should fail before chart loading: %v", err)
	}
}

func TestSystemArtifactRoleRender(t *testing.T) {
	source := releases.SystemNetworking{Chart: "dpu-networking", ChartVersion: "v26.8.0"}
	root := &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion: "v2", Name: source.Chart, Version: source.ChartVersion, AppVersion: source.ChartVersion,
			Annotations: map[string]string{"dpfVersion": source.ChartVersion},
		},
		Raw: []*chart.File{{Name: chartutil.ValuesfileName, Data: []byte("alpha:\n  enabled: false\nbeta:\n  enabled: false\n")}},
	}
	for _, name := range []string{"alpha", "beta"} {
		root.Metadata.Dependencies = append(root.Metadata.Dependencies, &chart.Dependency{
			Name: name, Version: "0.1.0", Repository: "file://charts/" + name, Condition: name + ".enabled",
		})
		root.AddDependency(&chart.Chart{
			Metadata:  &chart.Metadata{APIVersion: "v2", Name: name, Version: "0.1.0"},
			Templates: []*chart.File{{Name: "templates/test.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n")}},
		})
	}
	path, err := chartutil.Save(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"alpha", "beta"} {
		objects, err := renderSystemArtifactRole(archive, source, systemArtifactRole{Component: component},
			map[string]any{component: map[string]any{"enabled": true}}, chartutil.DefaultCapabilities.KubeVersion)
		if err != nil || len(objects) != 1 || objects[0].GetName() != component {
			t.Fatalf("%s should render only its dependency, got %v, %v", component, objects, err)
		}
	}
	for _, test := range []struct {
		name      string
		version   string
		component string
		values    map[string]any
		wantErr   string
	}{
		{name: "unexpected version", version: "v26.4.0", component: "alpha", values: map[string]any{"alpha": map[string]any{"enabled": true}}, wantErr: "metadata"},
		{name: "absent role", version: source.ChartVersion, component: "gamma", values: map[string]any{"gamma": map[string]any{"enabled": true}}, wantErr: "component"},
		{name: "second dependency", version: source.ChartVersion, component: "alpha", values: map[string]any{"alpha": map[string]any{"enabled": true}, "beta": map[string]any{"enabled": true}}, wantErr: "component"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock := source
			lock.ChartVersion = test.version
			_, err := renderSystemArtifactRole(archive, lock, systemArtifactRole{Component: test.component}, test.values, chartutil.DefaultCapabilities.KubeVersion)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected %s error, got %v", test.wantErr, err)
			}
		})
	}
}

func TestSystemArtifactCRDRejectsConflictingDefinitions(t *testing.T) {
	objects, err := decodeOperatorManifest([]byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: pools.example.test
spec:
  group: example.test
  names:
    kind: Pool
    plural: pools
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`))
	if err != nil {
		t.Fatal(err)
	}
	definitions := map[string]*unstructured.Unstructured{}
	first := objects[0]
	if err := addSystemArtifactCRD(definitions, first); err != nil {
		t.Fatal(err)
	}
	if err := addSystemArtifactCRD(definitions, first.DeepCopy()); err != nil || len(definitions) != 1 {
		t.Fatalf("identical same-name CRD should be deduplicated, got %v", err)
	}
	conflict := first.DeepCopy()
	if err := unstructured.SetNestedField(conflict.Object, "ChangedPool", "spec", "names", "kind"); err != nil {
		t.Fatal(err)
	}
	if err := addSystemArtifactCRD(definitions, conflict); err == nil || !strings.Contains(err.Error(), "conflicting") || definitions[first.GetName()] != first {
		t.Fatalf("expected original definition to survive conflicting duplicate, got %v", err)
	}
}

func TestSystemArtifactValuesUseRetainedIntent(t *testing.T) {
	release, err := releases.Resolve("26.8.0")
	if err != nil {
		t.Fatal(err)
	}
	intent := generation.ResolvedIntent{
		Networking: hbn.Profile{UplinkMTU: 8123},
		Bootstrap: generation.ResolvedBootstrap{
			PodCIDR: "10.44.0.0/16",
			Tenant:  bootstrap.Tenant{NodeSelector: map[string]string{"example.test/control-plane": "true"}},
		},
	}
	for _, test := range []struct {
		role       systemArtifactRole
		perCluster bool
		path       []string
		value      any
	}{
		{role: systemArtifactRole{Component: "flannel"}, path: []string{"flannel", "podCidr"}, value: "10.44.0.0/16"},
		{role: systemArtifactRole{Component: "flannel"}, path: []string{"flannel", "flannel", "skipCNIConfigInstallation"}, value: true},
		{role: systemArtifactRole{Component: "multus"}, path: []string{"multus", "mtu"}, value: "8123"},
		{role: systemArtifactRole{Component: "coredns", Management: true}, perCluster: true, path: []string{"coredns", "nodeSelector", "example.test/control-plane"}, value: "true"},
		{role: systemArtifactRole{Component: "coredns", Management: true}, perCluster: true, path: []string{"coredns", "deployHostManifests"}, value: true},
		{role: systemArtifactRole{Component: "coredns"}, perCluster: true, path: []string{"coredns", "deployDPUManifests"}, value: true},
	} {
		values := systemArtifactValues(intent, release, test.role, test.perCluster)
		value, found, err := unstructured.NestedFieldNoCopy(values, test.path...)
		if err != nil || !found || value != test.value {
			t.Fatalf("%v = %v, present %v, error %v", test.path, value, found, err)
		}
	}
	roles := systemArtifactRoles()
	unique := make(map[systemArtifactRole]bool)
	for _, role := range roles {
		unique[role] = true
	}
	if len(roles) != 15 || len(unique) != len(roles) {
		t.Fatalf("system role coverage = %d entries, %d distinct", len(roles), len(unique))
	}
}

func TestSystemArtifactImagesPreserveContainerRole(t *testing.T) {
	workload := &unstructured.Unstructured{Object: map[string]any{}}
	if err := unstructured.SetNestedSlice(workload.Object, []any{map[string]any{"name": "agent", "image": "example.test/agent:v1"}}, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(workload.Object, []any{map[string]any{"name": "setup", "image": "example.test/setup:v1"}}, "spec", "template", "spec", "initContainers"); err != nil {
		t.Fatal(err)
	}
	images, err := systemArtifactImages(workload)
	expected := map[systemArtifactContainer]string{
		{Kind: "containers", Name: "agent"}:     "example.test/agent:v1",
		{Kind: "initContainers", Name: "setup"}: "example.test/setup:v1",
	}
	if err != nil || !reflect.DeepEqual(images, expected) {
		t.Fatalf("got %v, %v", images, err)
	}
	if err := unstructured.SetNestedSlice(workload.Object, []any{map[string]any{"name": "setup", "image": "example.test/setup:v1"}, map[string]any{"name": "setup", "image": "example.test/alternate:v1"}}, "spec", "template", "spec", "initContainers"); err != nil {
		t.Fatal(err)
	}
	if _, err := systemArtifactImages(workload); err == nil {
		t.Fatal("expected error for duplicate init container identity")
	}
}

func TestSystemArtifactAttachmentsRemainExpectedWhenAppInventoryIsMissing(t *testing.T) {
	total := 0
	for _, role := range systemArtifactRoles() {
		names := systemArtifactAttachmentNames(role)
		total += len(names)
		if role.Management && len(names) != 0 {
			t.Fatal("management role unexpectedly requires a tenant attachment")
		}
		for _, name := range names {
			if role.Component == "multus" && name != "mybrhbn" || role.Component == "sfc-controller" && name != "iprequest" {
				t.Fatal("selected attachment identity differs")
			}
		}
	}
	if total != 2 {
		t.Fatalf("expected selected HBN attachment completeness is %d instead of two", total)
	}
}
