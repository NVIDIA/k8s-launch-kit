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

package networkoperatorplugin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// TestIPPoolReservedExclusionsRenderPerRail verifies the reserve-count pattern
// is applied to each auto-generated per-rail subnet: every rendered IPPool doc
// carries a spec.exclusions block whose low range starts at that rail's own
// network address.
func TestIPPoolReservedExclusionsRenderPerRail(t *testing.T) {
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
	require.NotNil(t, cfg.NvIpam)
	cfg.NvIpam.ReserveFirstIPs = 10
	cfg.NvIpam.ReserveLastIPs = 6

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
	require.NoError(t, err)

	names := fileNamesMatching(rendered, "ippool")
	require.NotEmpty(t, names, "expected at least one IPPool manifest")

	sawDoc := false
	for _, name := range names {
		for _, doc := range parseDocs(t, name, rendered[name]) {
			spec, ok := doc["spec"].(map[string]any)
			require.True(t, ok)
			subnet, ok := spec["subnet"].(string)
			require.True(t, ok)

			ex, ok := spec["exclusions"].([]any)
			require.Truef(t, ok, "IPPool for subnet %s is missing exclusions", subnet)
			require.Lenf(t, ex, 2, "expected low+high exclusion ranges for subnet %s", subnet)

			networkIP := strings.SplitN(subnet, "/", 2)[0]
			low, ok := ex[0].(map[string]any)
			require.True(t, ok)
			// Low range begins at the rail's own network address (.0).
			assert.Equal(t, networkIP, low["startIP"], "low exclusion must start at the subnet network address")
			assert.NotEmpty(t, low["endIP"])

			high, ok := ex[1].(map[string]any)
			require.True(t, ok)
			assert.NotEmpty(t, high["startIP"])
			assert.NotEmpty(t, high["endIP"])
			sawDoc = true
		}
	}
	require.True(t, sawDoc, "expected at least one IPPool document with exclusions")
}

// TestIPPoolNoExclusionsWhenReserveUnset confirms the default (no reserve, no
// explicit exclusions) renders no exclusions block — byte-compatible with the
// pre-feature output.
func TestIPPoolNoExclusionsWhenReserveUnset(t *testing.T) {
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
	require.NoError(t, err)

	for _, name := range fileNamesMatching(rendered, "ippool") {
		assert.NotContains(t, rendered[name], "exclusions:",
			"IPPool should have no exclusions block when reserve is unset")
	}
}
