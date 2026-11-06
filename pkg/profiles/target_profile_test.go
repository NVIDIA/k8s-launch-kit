// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package profiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/stretchr/testify/require"
)

func TestFindApplicableProfileSkipsDPFPayloadBeforeHostDecode(t *testing.T) {
	root := t.TempDir()
	writeTestProfile(t, root, "host-profile", `name: Host profile
plugin: test-plugin
profileRequirements:
  fabric: ethernet
  deployment: sriov
nodeCapabilities:
  rdma: true
templates:
  - host.yaml
`, map[string]string{"host.yaml": "kind: Host\n"})
	writeTestProfile(t, root, "dpf-profile", `target: dpf
name: DPF profile
plugin: test-plugin
profileRequirements: this is deliberately not a Host requirements mapping
templates:
  - ../outside.yaml
`, nil)

	profile, err := findApplicableProfile(root,
		&config.Profile{Fabric: "ethernet", Deployment: "sriov"},
		&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
		"test-plugin", "", config.FlavorK8s)
	require.NoError(t, err)
	require.Equal(t, "Host profile", profile.Name)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(resolvedRoot, "host-profile", "host.yaml")}, profile.Templates)
}

func TestLoadProfileForTargetDefaultsMissingTargetToHost(t *testing.T) {
	root := t.TempDir()
	writeTestProfile(t, root, "host-profile", `name: Host profile
plugin: test-plugin
templates:
  - host.yaml
`, map[string]string{"host.yaml": "kind: Host\n"})

	metadata, err := loadProfileForTarget(root, target.Host, "host-profile")
	require.NoError(t, err)
	require.Equal(t, target.Host, metadata.Target)
	require.Equal(t, "Host profile", metadata.Name)
	require.Equal(t, "test-plugin", metadata.Plugin)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(resolvedRoot, "host-profile", "host.yaml")}, metadata.TemplatePaths)
	require.Contains(t, string(metadata.Manifest), "name: Host profile")
}

func TestDecodeProfileTargetRejectsUnknownInvalidAndMalformedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{name: "unknown", manifest: "target: future\n", wantErr: `unknown or invalid target "future"`},
		{name: "invalid type", manifest: "target: [host]\n", wantErr: "invalid target header"},
		{name: "null is not omission", manifest: "target: null\n", wantErr: "invalid target header"},
		{name: "duplicate", manifest: "target: host\ntarget: dpf\n", wantErr: "duplicate target header"},
		{name: "malformed yaml", manifest: "target: dpf\nname: [\n", wantErr: "decode target header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProfileTarget([]byte(tc.manifest))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestLoadProfileForTargetRequiresExactProfileAndMatchingTarget(t *testing.T) {
	root := t.TempDir()
	writeTestProfile(t, root, "dpf-profile", `target: dpf
name: DPF profile
plugin: dpf
templates:
  - dpf.yaml
`, map[string]string{"dpf.yaml": "kind: DPF\n"})

	_, err := loadProfileForTarget(root, target.DPF, "missing")
	require.ErrorContains(t, err, `profile "missing" not found`)
	_, err = loadProfileForTarget(root, target.Host, "dpf-profile")
	require.ErrorContains(t, err, `targets "dpf", not "host"`)
	_, err = loadProfileForTarget(root, target.DPF, "../dpf-profile")
	require.ErrorContains(t, err, "invalid profile name")
	_, err = loadProfileForTarget(root, target.Name("future"), "dpf-profile")
	require.ErrorContains(t, err, `unknown profile target "future"`)
}

func TestLoadProfileForTargetRejectsTemplateTraversalAndSymlinkEscape(t *testing.T) {
	t.Run("traversal", func(t *testing.T) {
		root := t.TempDir()
		writeTestProfile(t, root, "dpf-profile", `target: dpf
name: DPF profile
templates:
  - ../../outside.yaml
`, nil)
		_, err := loadProfileForTarget(root, target.DPF, "dpf-profile")
		require.ErrorContains(t, err, "path escapes profiles directory")
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.yaml")
		require.NoError(t, os.WriteFile(outside, []byte("kind: Outside\n"), 0o600))
		profileDir := filepath.Join(root, "dpf-profile")
		require.NoError(t, os.MkdirAll(profileDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(profileDir, "profile.yaml"), []byte(`target: dpf
name: DPF profile
templates:
  - outside.yaml
`), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(profileDir, "outside.yaml")))
		_, err := loadProfileForTarget(root, target.DPF, "dpf-profile")
		require.ErrorContains(t, err, "path escapes profiles directory")
	})
}

func writeTestProfile(t *testing.T, root, name, manifest string, files map[string]string) {
	t.Helper()
	directory := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "profile.yaml"), []byte(manifest), 0o600))
	for path, contents := range files {
		require.NoError(t, os.WriteFile(filepath.Join(directory, path), []byte(contents), 0o600))
	}
}
