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

package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveKubeconfigPrecedence(t *testing.T) {
	statExisting := func(string) (os.FileInfo, error) { return nil, nil }
	statMissing := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	actual, err := resolveKubeconfig("/flag", "/home", func(string) string { return "/env" }, statExisting)
	require.NoError(t, err)
	assert.Equal(t, "/flag", actual)

	actual, err = resolveKubeconfig("", "/home", func(string) string { return "/env" }, statExisting)
	require.NoError(t, err)
	assert.Equal(t, "/env", actual)

	actual, err = resolveKubeconfig("", "/home", func(string) string { return "" }, statExisting)
	require.NoError(t, err)
	assert.Equal(t, "/home", actual)

	actual, err = resolveKubeconfig("", "/home", func(string) string { return "" }, statMissing)
	assert.Empty(t, actual)
	assert.ErrorContains(t, err, "no kubeconfig found")
}

func TestResolveDeploymentDirPrefersHostSubdirectory(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, manifestSubdir)
	require.NoError(t, os.Mkdir(hostDir, 0o755))

	actual, err := ResolveDeploymentDir(root)
	require.NoError(t, err)
	assert.Equal(t, hostDir, actual)

	require.NoError(t, os.Remove(hostDir))
	actual, err = ResolveDeploymentDir(root)
	require.NoError(t, err)
	assert.Equal(t, root, actual)
}

func TestUserConfigPathForUsesExplicitAndDeploymentFallback(t *testing.T) {
	root := t.TempDir()
	deployment := filepath.Join(root, "deployment")
	require.NoError(t, os.Mkdir(deployment, 0o755))
	configPath := filepath.Join(root, "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("clusterConfig: []\n"), 0o600))

	actual, err := UserConfigPathFor(UserConfigInput{Explicit: "/explicit"})
	require.NoError(t, err)
	assert.Equal(t, "/explicit", actual)

	t.Chdir(t.TempDir())
	actual, err = UserConfigPathFor(UserConfigInput{DeploymentFiles: deployment})
	require.NoError(t, err)
	assert.Equal(t, configPath, actual)
}
