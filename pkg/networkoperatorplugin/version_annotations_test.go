// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestGeneratedResourcesCarryLaunchKitVersionAnnotation(t *testing.T) {
	profilesUnderTest := []struct {
		dir             string
		fabric          string
		deployment      string
		spcxVersion     string
		selectedRelease string
	}{
		{dir: "host-device-rdma", fabric: "ethernet", deployment: "host_device"},
		{dir: "ipoib-rdma-shared", fabric: "infiniband", deployment: "rdma_shared"},
		{dir: "macvlan-rdma-shared", fabric: "ethernet", deployment: "rdma_shared"},
		{dir: "sriov-ethernet-rdma", fabric: "ethernet", deployment: "sriov"},
		{dir: "sriov-ib-rdma", fabric: "infiniband", deployment: "sriov"},
		{dir: "spectrum-x-ra2.1", fabric: "ethernet", deployment: "sriov", spcxVersion: "RA2.1", selectedRelease: "26.1"},
		{dir: "spectrum-x-ra2.2", fabric: "ethernet", deployment: "sriov", spcxVersion: "RA2.2", selectedRelease: "26.4"},
		{dir: "spectrum-x", fabric: "ethernet", deployment: "sriov", spcxVersion: "RA2.3", selectedRelease: "26.7"},
	}

	for _, profile := range profilesUnderTest {
		profile := profile
		t.Run(profile.dir, func(t *testing.T) {
			ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
			cfg, err := config.LoadFullConfig(
				filepath.Join("testdata", "grouping", "mixed-same-type.yaml"),
				ctrllog.Log,
			)
			require.NoError(t, err)
			cfg.Profile = &config.Profile{
				Fabric:     profile.fabric,
				Deployment: profile.deployment,
				Multirail:  true,
			}
			if profile.spcxVersion != "" {
				cfg.NetworkOperator.SelectedRelease = profile.selectedRelease
				cfg.Profile.SpectrumX = &config.ProfileSpectrumX{
					Enable:         true,
					SPCXVersion:    profile.spcxVersion,
					MultiplaneMode: "swplb",
					NumberOfPlanes: 2,
					TopologyType:   config.SpectrumXTopology2Tier,
					TopologyFile:   writeSpectrumXTopology(t, cfg, 2),
					ConfigMapName:  "test-spectrum-x-profile",
					Profile:        "useSoftwareCCAlgorithm: true\n",
				}
			}

			rendered, err := (&NetworkOperatorPlugin{LaunchKitVersion: testLaunchKitVersion}).GenerateProfileDeploymentFiles(
				loadProfileFromDir(t, profile.dir),
				cfg,
			)
			require.NoError(t, err)
			require.NotEmpty(t, rendered)

			for filename, content := range rendered {
				if filename == helmValuesOutputName {
					assert.NotContains(t, content, launchKitVersionAnnotation,
						"values.yaml is Helm input, not a Kubernetes resource")
					continue
				}
				resourceCount := strings.Count(content, "\nkind:")
				require.Positivef(t, resourceCount, "rendered file %s", filename)
				assert.Equalf(t, resourceCount,
					countVersionAnnotations(t, []byte(content), testLaunchKitVersion),
					"every document in %s must carry the Launch Kit version", filename)
			}
		})
	}
}
