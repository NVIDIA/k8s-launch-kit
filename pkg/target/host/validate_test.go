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

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

func TestValidateWritesPartialReportBeforeKubernetesClientExists(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "partial-report.html")
	runner := NewValidateRunner()

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      filepath.Join(t.TempDir(), "kubeconfig"),
		DeploymentFiles: filepath.Join(t.TempDir(), "missing-deployment"),
		ReportPath:      reportPath,
		OutputFormat:    "text",
		Version:         "test",
	})

	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
	assert.FileExists(t, reportPath)
}

func TestListNodesForReportAcceptsNilClient(t *testing.T) {
	assert.Nil(t, listNodesForReport(context.Background(), nil))
}

func TestValidateDoesNotCreateMissingDeploymentDirectoryForDefaultReport(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	deploymentPath := filepath.Join(workingDir, "missing-deployment")

	err := NewValidateRunner().Run(context.Background(), ValidateRequest{
		Kubeconfig:      filepath.Join(workingDir, "missing-kubeconfig"),
		DeploymentFiles: deploymentPath,
		OutputFormat:    "text",
	})

	require.Error(t, err)
	assert.NoDirExists(t, deploymentPath)
}
