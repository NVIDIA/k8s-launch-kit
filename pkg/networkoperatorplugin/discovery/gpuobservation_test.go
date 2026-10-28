// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gpuTestQuery = "0, 00000000:80:00.0\n1, 00000000:a0:00.0\n"
const gpuTestMatrix = `        GPU0 GPU1 NIC0 NIC1 CPU Affinity NUMA Affinity
GPU0    X SYS PIX PIX 0-31 0
GPU1    SYS X NODE NODE 32-63 1
NIC0    PIX NODE X PIX
NIC1    PIX NODE PIX X
Legend:
  X = Self
NIC Legend:
  NIC0: mlx5_local0
  NIC1: mlx5_local1
`

func gpuTestOutput(query, matrix string) string {
	return "---PRODUCT---\nProduct Name : NVIDIA Test GPU\n---PRODUCT-STATUS---\n0\n---QUERY---\n" + query +
		"---QUERY-STATUS---\n0\n---TOPO---\n" + matrix + "---TOPO-STATUS---\n0\n---DONE---\n"
}

func gpuTestHostOutput() string {
	return "GPU\t0000:80:00.0\nGPU\t0000:a0:00.0\nHOSTDONE\t2\nNIC\t0000:18:00.0\tmlx5_local0\nNIC\t0000:18:00.1\tmlx5_local1\nDONE\n"
}

func TestGPUObservationRequiresIndependentCompleteCoverage(t *testing.T) {
	host, err := parseGPUHostObservation(gpuTestHostOutput())
	require.NoError(t, err)
	good := gpuTestOutput(gpuTestQuery, gpuTestMatrix)
	product, data, err := parseGPUObservation(good, host)
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA-Test-GPU", product)
	assert.Equal(t, "0000:80:00.0", data.gpuPCI[0])
	assert.Equal(t, proxPIX, data.matrix["mlx5_local0"][0])
	for name, output := range map[string]string{
		"missing binary":         "---UNAVAILABLE---\n",
		"query failed":           strings.Replace(good, "---QUERY-STATUS---\n0", "---QUERY-STATUS---\n1", 1),
		"matrix failed":          strings.Replace(good, "---TOPO-STATUS---\n0", "---TOPO-STATUS---\n1", 1),
		"truncated":              strings.Replace(good, "---DONE---", "", 1),
		"restricted query":       gpuTestOutput("0, 0000:80:00.0\n", gpuTestMatrix),
		"duplicate index":        gpuTestOutput(gpuTestQuery+"0, 0000:a0:00.0\n", gpuTestMatrix),
		"duplicate physical pci": gpuTestOutput("0, 0000:80:00.0\n1, 0000:80:00.0\n", gpuTestMatrix),
		"unknown physical pci":   strings.Replace(good, "00000000:a0:00.0", "00000000:b0:00.0", 1),
		"missing GPU row":        strings.Replace(good, "GPU1    SYS X NODE NODE 32-63 1\n", "", 1),
		"duplicate GPU row":      strings.Replace(good, "NIC0    PIX", "GPU0 X SYS PIX PIX\nNIC0    PIX", 1),
		"extra GPU header":       strings.Replace(good, "GPU0 GPU1 NIC0", "GPU0 GPU1 GPU2 NIC0", 1),
		"duplicate NIC alias":    strings.Replace(good, "NIC1: mlx5_local1", "NIC1: mlx5_local0", 1),
		"missing NIC legend":     strings.Replace(good, "  NIC1: mlx5_local1\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseGPUObservation(output, host)
			require.Error(t, err)
		})
	}
	product, _, err = parseGPUObservation(strings.Replace(good, "---TOPO-STATUS---\n0", "---TOPO-STATUS---\n1", 1), host)
	require.Error(t, err)
	assert.Equal(t, "NVIDIA-Test-GPU", product, "topology failure must preserve a separately valid product")
	product, _, err = parseGPUObservation(strings.Replace(good, "---PRODUCT-STATUS---\n0", "---PRODUCT-STATUS---\n1", 1), host)
	require.NoError(t, err)
	assert.Empty(t, product, "product failure must not invalidate complete topology")
}

func TestGPUHostObservationDistinguishesEmptyFromIncomplete(t *testing.T) {
	host, err := parseGPUHostObservation("HOSTDONE\t0\nDONE\n")
	require.NoError(t, err)
	assert.Empty(t, host.gpus)
	for _, output := range []string{"", "HOSTDONE\t0\n", "GPU\t0000:80:00.0\nDONE\n", "HOSTDONE\t1\nDONE\n", gpuTestHostOutput() + "GPU\t0000:b0:00.0\n"} {
		_, err := parseGPUHostObservation(output)
		require.Error(t, err)
	}
	duplicate := strings.Replace(gpuTestHostOutput(), "mlx5_local1", "mlx5_local0", 1)
	host, err = parseGPUHostObservation(duplicate)
	require.NoError(t, err)
	assert.Empty(t, host.rdma["0000:18:00.0"])
	assert.Empty(t, host.rdma["0000:18:00.1"])
}

func TestGPUHostProbeReadsPhysicalPCIAndLocalAliases(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(value), 0600))
	}
	write("0000:80:00.0/vendor", "0x10de\n")
	write("0000:80:00.0/class", "0x030200\n")
	write("0000:81:00.0/vendor", "0x10de\n")
	write("0000:81:00.0/class", "0x068000\n") // NVSwitch is not a GPU.
	write("0000:18:00.0/vendor", "0x15b3\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "0000:18:00.0/infiniband/renamed_rdma"), 0755))
	command := exec.Command("/bin/sh", "-c", gpuHostProbeCmd, "l8k-gpu-host", root, "0000:18:00.0")
	output, err := command.CombinedOutput()
	t.Logf("host GPU probe: %s", output)
	require.NoError(t, err)
	host, err := parseGPUHostObservation(string(output))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"0000:80:00.0": true}, host.gpus)
	assert.Equal(t, "renamed_rdma", host.rdma["0000:18:00.0"])
	require.NoError(t, os.Remove(filepath.Join(root, "0000:80:00.0/class")))
	output, err = exec.Command("/bin/sh", "-c", gpuHostProbeCmd, "l8k-gpu-host", root).CombinedOutput()
	t.Logf("incomplete host GPU probe: %s", output)
	require.Error(t, err, "unreadable classification cannot prove an empty host")
}

func TestGPUProbeUsesInjectedExecutableAndIndependentStatuses(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n-q) echo 'Product Name : NVIDIA Test GPU';;\n--query-gpu=*) printf '0, 00000000:80:00.0\\n';;\ntopo) echo 'NVML failed' >&2; exit 7;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0700))
	command := exec.Command("/bin/sh", "-c", gpuObservationCmd)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	t.Logf("injected GPU probe: %s", output)
	require.NoError(t, err)
	assert.Contains(t, string(output), "---PRODUCT-STATUS---\n0")
	assert.Contains(t, string(output), "---QUERY-STATUS---\n0")
	assert.Contains(t, string(output), "---TOPO-STATUS---\n7")
	assert.Contains(t, string(output), "---DONE---")
}
