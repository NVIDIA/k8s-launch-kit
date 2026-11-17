// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/internal/dpftest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func TestApp(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF application") }

func generationSite() config.Input {
	input, err := dpftest.GenerationSite()
	Expect(err).NotTo(HaveOccurred())
	return input
}

func publicFixtureBytes(input config.Input) []byte {
	data, err := dpftest.PublicInventoryBytes(input)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// Retained-plan tests exercise application resolution without the native adapter;
// target tests separately cover CLI publication and process cancellation.
func generatedUseCaseSite(input config.Input) (config.Input, *bundle.Bundle) {
	path := filepath.Join(GinkgoT().TempDir(), "site.yaml")
	data, err := yaml.Marshal(input.Site)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(path, data, 0600)).To(Succeed())
	loaded, err := config.Decode(data, path)
	Expect(err).NotTo(HaveOccurred())
	resolved, err := ResolveGeneration(loaded, nil)
	Expect(err).NotTo(HaveOccurred())
	resolved.OperatorArchive, err = os.ReadFile("../bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
	Expect(err).NotTo(HaveOccurred())
	resolved.HBNChartArchive, err = os.ReadFile("../generation/testdata/doca-hbn-3.5.0.tgz")
	Expect(err).NotTo(HaveOccurred())
	retained, err := generation.Generate(resolved.Inputs)
	Expect(err).NotTo(HaveOccurred())
	return loaded, retained
}
