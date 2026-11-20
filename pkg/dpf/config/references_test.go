// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	"gopkg.in/yaml.v3"
)

func TestReferencedFilesSharesPublicationRulesWithoutReadingCredentials(t *testing.T) {
	root := t.TempDir()
	const source = `management:
  kubeconfigRef: {file: access/kubeconfig}
credentials:
  unused:
    privateKeyRef: {file: access/key}
  bmc:
    passwordRef: {file: access/password}
discovery:
  tls: {caFile: access/ca}
  hosts:
    - ssh: {knownHostsFile: access/known_hosts}
  addressLookup:
    sources:
      - file: allocations.csv
inventory:
  privateKeyRef: {file: not-an-access-reference}
`
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(source), &document); err != nil {
		t.Fatal(err)
	}
	before, err := yamlinput.Encode(&document)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Path: filepath.Join(root, "inventory.yaml"), OverridesPath: filepath.Join(root, "overrides.yaml"), Document: &document}
	files, err := ReferencedFiles(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"inventory.yaml", "overrides.yaml", "access/kubeconfig", "access/key", "access/password", "access/ca", "access/known_hosts", "allocations.csv"}
	if len(files) != len(want) {
		t.Fatalf("unexpected reference set: %v", files)
	}
	for i, path := range want {
		if files[i] != filepath.Join(root, path) {
			t.Fatalf("reference %d = %q, want %q", i, files[i], path)
		}
	}
	after, err := yamlinput.Encode(&document)
	if err != nil || string(before) != string(after) {
		t.Fatalf("reference enumeration mutated input: %v", err)
	}
}
