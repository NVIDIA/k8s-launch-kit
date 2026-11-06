// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestENETPresetRDMASharedGeneration(t *testing.T) {
	const presetName = "MGX-ETL256-Vera-NVIDIA-Graphics-Device-ENET"
	catalog, err := presets.EmbeddedCatalog()
	require.NoError(t, err)
	topology, err := catalog.LoadPresetByDir(presetName)
	require.NoError(t, err)
	group, err := presets.SynthesizeClusterConfig(presetName, topology,
		map[string]string{"example.com/node-group": "enet"})
	require.NoError(t, err)
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.ClusterConfig = []config.ClusterConfig{group}
	cfg.Profile = &config.Profile{
		Fabric: "ethernet", Deployment: "rdma_shared", Multirail: true,
		Routing: "source-based", IgnoreARP: true,
	}
	cfg.NetworkOperator.SelectedRelease = "26.10"
	cfg.Validation.GPUDirect.Enabled = false
	before, err := config.CloneConfig(cfg)
	require.NoError(t, err)

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "macvlan-rdma-shared"), cfg)
	require.NoError(t, err)
	assert.Equal(t, before, cfg, "generation must preserve preset inventory and saved names")
	assert.Empty(t, fileNamesMatching(rendered, "nicinterfacenametemplate"),
		"one homogeneous source with saved interface names must not rename its PFs")

	files := make([]bundle.File, 0, len(rendered))
	for name, content := range rendered {
		assert.NotContains(t, content, "0005:05:00.0")
		assert.NotContains(t, content, "enP5p5s0np0")
		files = append(files, bundle.File{Name: name, Content: content})
	}
	artifacts, err := bundle.FromFiles(files)
	require.NoError(t, err)

	masters := map[string]string{}
	var deviceConfig struct {
		ConfigList []struct {
			ResourceName string `json:"resourceName"`
			Selectors    struct {
				IfNames []string `json:"ifNames"`
			} `json:"selectors"`
		} `json:"configList"`
	}
	for _, doc := range artifacts.Documents() {
		object := doc.ObjectCopy()
		switch object.GetKind() {
		case "MacvlanNetwork":
			master, found, fieldErr := unstructured.NestedString(object.Object, "spec", "master")
			require.NoError(t, fieldErr)
			require.True(t, found)
			_, duplicate := masters[master]
			assert.False(t, duplicate, "one network per PF")
			masters[master] = object.GetName()
			plugins, found, fieldErr := unstructured.NestedString(object.Object, "spec", "metaPlugins")
			require.NoError(t, fieldErr)
			require.True(t, found)
			var chain []struct {
				Type string `json:"type"`
			}
			// Network Operator accepts comma-separated plugin objects.
			require.NoError(t, json.Unmarshal([]byte("["+plugins+"]"), &chain))
			require.Len(t, chain, 2)
			assert.Equal(t, "tuning", chain[0].Type)
			assert.Equal(t, "sbr", chain[1].Type)
		case "NicNodePolicy":
			assert.Empty(t, deviceConfig.ConfigList, "one policy for the synthesized source")
			payload, found, fieldErr := unstructured.NestedString(object.Object, "spec", "rdmaSharedDevicePlugin", "config")
			require.NoError(t, fieldErr)
			require.True(t, found)
			require.NoError(t, json.Unmarshal([]byte(payload), &deviceConfig))
		}
	}
	require.Len(t, masters, 16)
	require.Len(t, deviceConfig.ConfigList, 16)
	for rail, pf := range group.PFs {
		assert.Contains(t, masters, pf.NetworkInterface)
		assert.Equal(t, fmt.Sprintf("%s_rail_%d", cfg.RdmaShared.ResourceName, rail),
			deviceConfig.ConfigList[rail].ResourceName)
		assert.Equal(t, []string{pf.NetworkInterface}, deviceConfig.ConfigList[rail].Selectors.IfNames)
	}
}
