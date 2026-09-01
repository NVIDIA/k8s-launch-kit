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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestSchemaDescribesTargetsAdditively(t *testing.T) {
	targets := targetCapabilitiesSchema()
	require.Len(t, targets, 2)

	host := targets[0]
	assert.Equal(t, string(target.Host), host.Name)
	assert.True(t, host.Default)
	dpf := targets[1]
	assert.Equal(t, string(target.DPF), dpf.Name)
	assert.False(t, dpf.Default)

	for _, phase := range target.PublicPhases() {
		assert.Truef(t, host.Phases[string(phase)].Available, "host phase %s", phase)
		assert.Falsef(t, dpf.Phases[string(phase)].Available, "dpf phase %s", phase)
		assert.NotEmptyf(t, dpf.Phases[string(phase)].Reason, "dpf phase %s reason", phase)
	}

}

func TestSchemaFlagsDeclareTargetOwnership(t *testing.T) {
	s := schema{Flags: map[string]flagSchema{
		"--target":                     {},
		"--output":                     {},
		"--dry-run":                    {},
		"--fabric":                     {},
		"--kubeconfig":                 {},
		"--skip-network-operator-helm": {},
	}}
	annotateSchemaFlagTargets(&s)
	for name, spec := range s.Flags {
		assert.NotEmptyf(t, spec.Targets, "schema flag %s", name)
	}

	assert.Equal(t, []string{"host", "dpf"}, s.Flags["--target"].Targets)
	assert.Equal(t, []string{"host", "dpf"}, s.Flags["--output"].Targets)
	assert.Equal(t, []string{"host", "dpf"}, s.Flags["--dry-run"].Targets)
	assert.Equal(t, []string{"host"}, s.Flags["--fabric"].Targets)
	assert.Equal(t, []string{"host"}, s.Flags["--kubeconfig"].Targets)
	assert.Equal(t, []string{"networkOperator.skipHelmChart"},
		s.Flags["--skip-network-operator-helm"].ConfigPaths)
	assert.Empty(t, s.Flags["--kubeconfig"].ConfigPaths)
}

func TestAnnotateSchemaFlagTargetsNilSafe(t *testing.T) {
	require.NotPanics(t, func() { annotateSchemaFlagTargets(nil) })
}
