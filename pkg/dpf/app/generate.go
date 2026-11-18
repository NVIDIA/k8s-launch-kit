// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// GenerationRequest selects public inventory and optional generation settings.
// Access references in those files are retained without being opened.
type GenerationRequest struct {
	Inventory  string
	UserConfig string
	UseCase    *string
	OutputPath string
}

// PrepareGeneration loads the public inputs and resolves desired artifacts.
// This file I/O never opens referenced credentials, trust or kubeconfig files.
func PrepareGeneration(r GenerationRequest) (ResolvedGeneration, error) {
	input, err := config.LoadInputs(r.Inventory, r.UserConfig)
	if err != nil {
		return ResolvedGeneration{}, err
	}
	input, err = config.WithUseCase(input, r.UseCase)
	if err != nil {
		return ResolvedGeneration{}, err
	}
	if err := config.VerifyRecordedConfiguration(input); err != nil {
		return ResolvedGeneration{}, err
	}
	return ResolveGeneration(input, nil)
}

// ChartFetcher retrieves one public chart coordinate. The default transport is
// bounded and redacts remote diagnostics; generation verifies archive integrity.
type ChartFetcher func(context.Context, string) ([]byte, error)

// Generate retrieves and verifies exact charts, then renders one immutable
// bundle. It performs artifact I/O; ResolveGeneration is the pure input boundary.
// Publishing the bundle remains the caller's explicit responsibility.
func Generate(ctx context.Context, resolved ResolvedGeneration, fetch ChartFetcher) (*bundle.Bundle, error) {
	if fetch == nil {
		fetch = DownloadChart
	}
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] Charts: retrieving the selected DPF operator archive")
	var err error
	resolved.OperatorArchive, err = fetch(ctx, resolved.OperatorChartURL)
	if err != nil {
		return nil, fmt.Errorf("retrieve DPF operator chart: %w", err)
	}
	progress.Info("[RUNNING] Charts: retrieving the selected HBN archive")
	hbn := resolved.Workload.Generation
	resolved.HBNChartArchive, err = fetch(ctx, strings.TrimSuffix(hbn.ChartRepository, "/")+"/charts/"+hbn.ChartName+"-"+hbn.ChartVersion+".tgz")
	if err != nil {
		return nil, fmt.Errorf("retrieve HBN chart: %w", err)
	}
	progress.Info("[RUNNING] Manifests: verifying archives and validating the complete resource bundle")
	return generation.Generate(resolved.Inputs)
}

// GenerationResult describes the published bundle without CLI formatting.
type GenerationResult struct {
	Publication    bundle.Publication
	RequiredChecks []string
}

// GenerateBundle resolves public inputs, verifies charts and publishes one bundle.
// A pre-canceled operation does not read input files or retrieve artifacts.
func GenerateBundle(ctx context.Context, request GenerationRequest, fetch ChartFetcher) (GenerationResult, error) {
	if err := ctx.Err(); err != nil {
		return GenerationResult{}, err
	}
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] Generation: resolving profile and selected discovery identities")
	resolved, err := PrepareGeneration(request)
	if err != nil {
		return GenerationResult{}, &PreparationError{Stage: GenerationInputs, Err: err}
	}
	artifacts, err := Generate(ctx, resolved, fetch)
	if err != nil {
		return GenerationResult{}, err
	}
	publication, err := artifacts.Publish(ctx, request.OutputPath)
	if err != nil {
		return GenerationResult{}, err
	}
	index := artifacts.Index()
	progress.Info("[PASS] Generation: %d stages saved to %s", len(index.Stages), publication.Path)
	return GenerationResult{Publication: publication, RequiredChecks: index.ExternalPrerequisites}, nil
}
