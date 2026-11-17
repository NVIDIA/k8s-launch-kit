// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/internal/dpftest"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func generationSite() config.Input {
	input, err := dpftest.GenerationSite()
	Expect(err).NotTo(HaveOccurred())
	return input
}

func deploymentSite() config.Input {
	input := generationSite()
	input.Site.Inventory.ScopeDigest = "same-scope"
	input.Site.Inventory.Hosts[0].RecordKey = "host-source"
	input.Site.Inventory.DPUs[0].RecordKey = "dpu-source"
	return input
}

func publicFixtureBytes(input config.Input) []byte {
	data, err := dpftest.PublicInventoryBytes(input)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// prepareFixtureGeneration supplies the existing resolved internal cohort to
// isolate publication, cancellation and retained-bundle tests from catalog
// support. Public-input tests exercise app.PrepareGeneration without this fixture.
func prepareFixtureGeneration(r app.GenerationRequest) (app.ResolvedGeneration, error) {
	input, err := config.Load(r.Inventory)
	if err != nil {
		return app.ResolvedGeneration{}, err
	}
	input, err = config.WithUseCase(input, r.UseCase)
	if err != nil {
		return app.ResolvedGeneration{}, err
	}
	return app.ResolveGeneration(input, nil)
}

var _ = Describe("DPF generation input boundary", func() {
	It("publishes a resolved test plan without reading access secrets", func() {
		input := generationSite()
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		siteBytes, err := yaml.Marshal(input.Site)
		Expect(err).NotTo(HaveOccurred())
		source := filepath.Join(root, "site.yaml")
		Expect(os.WriteFile(source, siteBytes, 0600)).To(Succeed())
		var stdout, stderr bytes.Buffer
		fetched := []string{}
		driver := generateDriver{request: GenerateRequest{Inventory: source, OutputPath: filepath.Join(root, "bundle"), Stdout: &stdout, Stderr: &stderr}, prepare: prepareFixtureGeneration, fetchChart: func(_ context.Context, address string) ([]byte, error) {
			fetched = append(fetched, address)
			if strings.HasSuffix(address, "dpf-operator-v26.8.0.tgz") {
				return os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
			}
			return os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
		}}
		operation, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "json"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(operation.Run(context.Background())).To(Succeed())
		Expect(fetched).To(Equal([]string{"https://helm.ngc.nvidia.com/nvidia/doca/charts/dpf-operator-v26.8.0.tgz", "https://helm.ngc.nvidia.com/nvidia/doca/charts/doca-hbn-3.5.0.tgz"}))
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["state"]).To(Equal("generated"))
		Expect(result["deploymentReadiness"]).To(Equal("not-evaluated"))
		loaded, err := bundle.Load(filepath.Join(root, "bundle"))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Index().Stages).To(HaveLen(7))
		saved, err := os.ReadFile(source)
		Expect(err).NotTo(HaveOccurred())
		Expect(saved).To(Equal(siteBytes))
		retained, err := os.ReadFile(filepath.Join(root, "bundle", ".l8k", "source-config.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(retained).To(Equal(siteBytes))
		stdout.Reset()
		Expect(operation.Run(context.Background())).To(Succeed())
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["unchanged"]).To(BeTrue())
	})

	It("rejects unsupported effects before reading input or fetching charts", func() {
		for _, inv := range []target.Invocation{
			{Target: target.DPF, Phase: target.Deploy},
			{Target: target.DPF, Phase: target.Generate, Execution: target.ExecutionOptions{DryRun: true}},
		} {
			_, err := NewGenerateAdapter(GenerateRequest{Inventory: "/missing", OutputPath: "/missing"}).Bind(inv)
			Expect(err).To(HaveOccurred())
			if inv.Phase == target.Deploy {
				_, phaseUnavailable := err.(*target.PhaseUnavailableError)
				Expect(phaseUnavailable).To(BeTrue())
			}
		}
		output := filepath.Join(GinkgoT().TempDir(), "bundle")
		op, err := NewGenerateAdapter(GenerateRequest{Inventory: "/missing", OutputPath: output, Deploy: true}).Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(op).To(BeNil())
		structured, ok := err.(*apperrors.StructuredError)
		Expect(ok).To(BeTrue())
		Expect(structured.Code).To(Equal("VALIDATION_ERROR"))
		Expect(structured.Category).To(Equal("validation"))
		Expect(structured.Transient).To(BeFalse())
		Expect(structured.ExitCode).To(Equal(apperrors.ExitValidation))
		Expect(structured.Message).To(Equal("DPF generate --deploy is unsupported"))
		Expect(structured.Suggestion).To(Equal("Generate and review the bundle first, then use 'l8k deploy --target dpf' with --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp"))
		serialized, marshalErr := json.Marshal(structured)
		Expect(marshalErr).NotTo(HaveOccurred())
		var fields map[string]any
		Expect(json.Unmarshal(serialized, &fields)).To(Succeed())
		Expect(fields["message"]).To(Equal("DPF generate --deploy is unsupported"))
		Expect(fields["suggestion"]).To(Equal("Generate and review the bundle first, then use 'l8k deploy --target dpf' with --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp"))
		_, statErr := os.Stat(output)
		Expect(os.IsNotExist(statErr)).To(BeTrue())
	})
	It("honors cancellation before file reads or archive downloads", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		driver := generateDriver{request: GenerateRequest{Inventory: "/must-not-open/site", OutputPath: "/must-not-create/output"}, fetchChart: func(context.Context, string) ([]byte, error) { called = true; return nil, nil }}
		op, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(ctx)).To(MatchError(context.Canceled))
		Expect(called).To(BeFalse())
	})
	It("does not fetch archives or publish when site resolution fails", func() {
		root := GinkgoT().TempDir()
		file := filepath.Join(root, "site.yaml")
		Expect(os.WriteFile(file, []byte("dpf: {useCase: hbn-bgp}\n"), 0600)).To(Succeed())
		called := false
		var stdout, stderr bytes.Buffer
		driver := generateDriver{request: GenerateRequest{Inventory: file, OutputPath: filepath.Join(root, "output"), Stdout: &stdout, Stderr: &stderr}, fetchChart: func(context.Context, string) ([]byte, error) { called = true; return nil, nil }}
		op, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(context.Background())).NotTo(Succeed())
		Expect(called).To(BeFalse())
		Expect(stdout.String()).To(BeEmpty())
		_, err = os.Stat(filepath.Join(root, "output"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
