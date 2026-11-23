// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package nvaie evaluates observed cluster facts against a bundled NVIDIA AI
// Enterprise infrastructure documentation snapshot.
package nvaie

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

const (
	NetworkOperator  = "Network Operator"
	GPUOperator      = "GPU Operator"
	GPUDriver        = "Data Center GPU Driver"
	ContainerToolkit = "Container Toolkit"
	DOCADriver       = "DOCA Driver for Networking"
	dpuOperator      = "DPU Operator (DPF)"
	vgpuManager      = "Virtual GPU Manager"
	vgpuGuest        = "vGPU for Compute Guest Driver"
)

type Source struct {
	MatrixURL   string `json:"matrixURL"`
	StaticURL   string `json:"staticURL"`
	DataURL     string `json:"dataURL"`
	RetrievedAt string `json:"retrievedAt"`
	IndexSHA256 string `json:"indexSHA256"`
	DataSHA256  string `json:"dataSHA256"`
}

type Component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	X86     bool   `json:"x86"`
	ARM     bool   `json:"arm"`
}

type GPU struct {
	Name                 string   `json:"name"`
	Type                 string   `json:"type"`
	Architecture         string   `json:"arch"`
	DeploymentExclusions []string `json:"deploymentExclusions,omitempty"`
}

type PlatformRow struct {
	Platform        string `json:"platform"`
	Version         string `json:"version"`
	Engine          string `json:"engine"`
	OS              string `json:"os"`
	GPUDriver       string `json:"gpuDriver"`
	GPUOperator     string `json:"gpuOp"`
	NetworkOperator string `json:"netOp"`
	DPUOperator     string `json:"dpuOp"`
	RunAI           string `json:"runai,omitempty"`
	Footnotes       []int  `json:"footnotes,omitempty"`
	NetOpFootnotes  []int  `json:"netOpFootnotes,omitempty"`
}

type Release struct {
	Version      string        `json:"version"`
	Components   []Component   `json:"components"`
	GPUs         []GPU         `json:"gpus"`
	BareMetalK8s []PlatformRow `json:"bareMetalK8s"`
}

// ComponentVersion returns the published pin or an empty string when absent.
func (r Release) ComponentVersion(name string) string {
	for _, component := range r.Components {
		if component.Name == name {
			return component.Version
		}
	}
	return ""
}

type Catalog struct {
	Schema   string    `json:"schema"`
	Source   Source    `json:"source"`
	Releases []Release `json:"releases"`
}

//go:embed catalog.json
var catalogJSON []byte

var bundledCatalog = sync.OnceValues(func() (Catalog, error) {
	return decodeCatalog(catalogJSON)
})

func decodeCatalog(data []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode bundled NVAIE catalog: %w", err)
	}
	if catalog.Schema != "l8k.nvaie.catalog.v1" {
		return Catalog{}, fmt.Errorf("unknown NVAIE catalog schema %q", catalog.Schema)
	}
	if len(catalog.Releases) == 0 || catalog.Source.DataURL == "" || catalog.Source.StaticURL == "" ||
		catalog.Source.RetrievedAt == "" || len(catalog.Source.DataSHA256) != 64 || len(catalog.Source.IndexSHA256) != 64 {
		return Catalog{}, fmt.Errorf("incomplete NVAIE catalog source or releases")
	}
	seen := map[string]bool{}
	for _, release := range catalog.Releases {
		if release.Version == "" || seen[release.Version] || len(release.Components) == 0 ||
			len(release.GPUs) == 0 || len(release.BareMetalK8s) == 0 {
			return Catalog{}, fmt.Errorf("missing, duplicate or incomplete NVAIE release %q", release.Version)
		}
		seen[release.Version] = true
		for _, component := range []string{NetworkOperator, GPUOperator, GPUDriver, ContainerToolkit, DOCADriver} {
			if release.ComponentVersion(component) == "" {
				return Catalog{}, fmt.Errorf("NVAIE release %s is missing the %s pin", release.Version, component)
			}
		}
	}
	return catalog, nil
}

// Lookup returns an independent copy of the exact infrastructure release.
func Lookup(version string) (Release, bool) {
	catalog, err := bundledCatalog()
	if err != nil {
		return Release{}, false
	}
	for _, release := range catalog.Releases {
		if release.Version == version {
			return cloneRelease(release), true
		}
	}
	return Release{}, false
}

func cloneRelease(release Release) Release {
	release.Components = append([]Component(nil), release.Components...)
	release.GPUs = append([]GPU(nil), release.GPUs...)
	for i := range release.GPUs {
		gpu := &release.GPUs[i]
		gpu.DeploymentExclusions = append([]string(nil), gpu.DeploymentExclusions...)
	}
	release.BareMetalK8s = append([]PlatformRow(nil), release.BareMetalK8s...)
	for i := range release.BareMetalK8s {
		row := &release.BareMetalK8s[i]
		row.Footnotes = append([]int(nil), row.Footnotes...)
		row.NetOpFootnotes = append([]int(nil), row.NetOpFootnotes...)
	}
	return release
}

// SupportedReleases lists exact release IDs present in the bundled catalog.
func SupportedReleases() []string {
	catalog, err := bundledCatalog()
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(catalog.Releases))
	for _, release := range catalog.Releases {
		result = append(result, release.Version)
	}
	sort.Strings(result)
	return result
}
