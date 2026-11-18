// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type chartSourceTransport struct {
	data  []byte
	calls int
}

func (t *chartSourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls++
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(t.data)), Request: request}, nil
}

var _ = Describe("complete DPF application operations", func() {
	It("stops generation before public input I/O when canceled and classifies preparation failure separately", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		fetch := func(context.Context, string) ([]byte, error) { calls++; return nil, nil }
		request := GenerationRequest{Inventory: "/must-not-read", OutputPath: filepath.Join(GinkgoT().TempDir(), "bundle")}
		result, err := GenerateBundle(ctx, request, fetch)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(result.Publication.Path).To(BeEmpty())
		Expect(calls).To(BeZero())
		_, err = GenerateBundle(context.Background(), request, fetch)
		var preparation *PreparationError
		Expect(errors.As(err, &preparation)).To(BeTrue())
		Expect(preparation.Stage).To(Equal(GenerationInputs))
		Expect(calls).To(BeZero())
		_, err = os.Stat(request.OutputPath)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("does not fetch the dependent chart or publish after the first chart is interrupted", func() {
		root := GinkgoT().TempDir()
		source := filepath.Join(root, "inventory.yaml")
		Expect(os.WriteFile(source, publicFixtureBytes(generationSite()), 0600)).To(Succeed())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		output := filepath.Join(root, "bundle")
		_, err := GenerateBundle(ctx, GenerationRequest{Inventory: source, OutputPath: output}, func(ctx context.Context, _ string) ([]byte, error) {
			calls++
			cancel()
			return nil, ctx.Err()
		})
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(calls).To(Equal(1))
		_, err = os.Stat(output)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("requires exact retained chart source bytes and preserves cancellation without a passed check", func() {
		_, retained := generatedUseCaseSite(generationSite())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		transport := &chartSourceTransport{data: plan.HBNChart()}
		previous := http.DefaultTransport
		http.DefaultTransport = transport
		DeferCleanup(func() { http.DefaultTransport = previous })
		check, err := verifyDeploymentChart(context.Background(), plan)
		Expect(err).NotTo(HaveOccurred())
		Expect(check.State).To(Equal("passed"))
		Expect(transport.calls).To(Equal(1))
		transport.data = append(bytes.Clone(transport.data), '\n')
		check, err = verifyDeploymentChart(context.Background(), plan)
		Expect(err).To(MatchError("current HBN chart source differs from the retained archive"))
		Expect(check.State).To(Equal("blocked"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		check, err = verifyDeploymentChart(ctx, plan)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(check.State).To(Equal("blocked"))
		check, err = verifyDeploymentChart(context.Background(), nil)
		Expect(err).To(MatchError("retained deployment plan is required"))
		Expect(check.State).To(Equal("blocked"))
	})
})
