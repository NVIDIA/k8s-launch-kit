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
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

func TestDeployRejectsMissingManifestDirectoryBeforeClusterAccess(t *testing.T) {
	err := NewDeployRunner().Run(context.Background(), DeployRequest{
		Kubeconfig:      filepath.Join(t.TempDir(), "kubeconfig"),
		DeploymentFiles: filepath.Join(t.TempDir(), "missing-deployment"),
		OutputFormat:    "text",
	})

	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
	assert.ErrorContains(t, err, "deployment files directory not found")
}

func TestSelectedReleaseFromConfigHandlesMissingValues(t *testing.T) {
	assert.Empty(t, selectedReleaseFromConfig(nil))
	assert.Empty(t, selectedReleaseFromConfig(&config.LaunchKitConfig{}))
	assert.Equal(t, "26.7", selectedReleaseFromConfig(&config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.7"},
	}))
}
