// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package generation composes pure resolution/rendering and atomic publication.
package generation

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

type Request struct {
	UserConfig string
	OutputPath string
	Overrides  configinput.Values
}

type Result struct {
	Outcome      string  `json:"outcome"`
	BundlePath   string  `json:"bundlePath"`
	BundleDigest string  `json:"bundleDigest"`
	Unchanged    bool    `json:"unchanged"`
	Checks       []Check `json:"checks"`
}

type Check struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	ReasonCode string `json:"reasonCode"`
	Message    string `json:"message"`
}

// Build returns a complete immutable snapshot without writing output, connecting
// to a cluster or opening any access credential reference.
func Build(ctx context.Context, in config.Input, overrides configinput.Values) (*bundle.Bundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	in, err := config.WithOverrides(in, overrides)
	if err != nil {
		return nil, err
	}
	g, err := config.ResolveGeneration(in)
	if err != nil {
		return nil, err
	}
	archives := map[string][]byte{}
	for _, chart := range sentry.Catalog() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		data, readErr := config.ReadReference(filepath.Join(g.Sentry.ChartDirectory, chart.Filename), 16<<20)
		if readErr != nil {
			return nil, fmt.Errorf("chart archive %s: %w", chart.Filename, readErr)
		}
		archives[chart.Name] = data
	}
	return renderBundle(ctx, g, archives)
}

// renderBundle is shared by generation and admission of a loaded bundle. It uses
// only resolved intent and retained chart bytes, never credentials or source paths.
func renderBundle(ctx context.Context, g config.Generation, archives map[string][]byte) (*bundle.Bundle, error) {
	sidecar, err := config.EncodeResolved(g)
	if err != nil {
		return nil, err
	}
	return renderBundleWithSidecar(ctx, g, archives, sidecar)
}

// Admission preserves the already decoded sidecar bytes, including legacy
// metadata, so a schema upgrade does not change the retained bundle digest.
func renderBundleWithSidecar(ctx context.Context, g config.Generation, archives map[string][]byte, sidecar []byte) (*bundle.Bundle, error) {
	p, err := profiles.LoadProfileForTarget(target.DPF, g.Profile)
	if err != nil {
		return nil, err
	}
	rendered, err := sentry.Render(g, p, archives)
	if err != nil {
		return nil, err
	}
	rendered.Files = append(rendered.Files, bundle.File{Path: bundle.ResolvedConfigPath, Data: sidecar})
	rendered.Records = append(rendered.Records, bundle.FileRecord{Path: bundle.ResolvedConfigPath, Role: bundle.ResolvedConfig})
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return bundle.New(bundle.Index{FormatVersion: bundle.FormatVersion, Target: bundle.Target, Phase: bundle.Phase, Profile: g.Profile, Release: g.Release, Renderer: sentry.RendererVersion, SelectedDevices: rendered.SelectedDevices, Files: rendered.Records, Stages: rendered.Stages, References: rendered.References, ExternalPrerequisites: rendered.ExternalPrerequisites}, rendered.Files)
}

func Run(ctx context.Context, r Request) (Result, error) {
	result := Result{Outcome: "failed"}
	fail := func(id string, err error) (Result, error) {
		result.Checks = append(result.Checks, Check{ID: id, State: "failed", ReasonCode: "GENERATION_FAILED", Message: err.Error()})
		return result, err
	}
	if r.OutputPath == "" {
		return fail("output", fmt.Errorf("--save-deployment-files is required for DPF generation"))
	}
	in, err := config.Load(r.UserConfig)
	if err != nil {
		return fail("configuration", err)
	}
	b, err := Build(ctx, in, r.Overrides)
	if err != nil {
		return fail("render", err)
	}
	publication, err := b.Publish(ctx, r.OutputPath)
	if err != nil {
		return fail("publication", err)
	}
	result.Outcome = "complete"
	result.BundlePath = publication.Path
	result.BundleDigest = b.Index().BundleDigest
	result.Unchanged = publication.Unchanged
	result.Checks = append(result.Checks, Check{ID: "generation", State: "passed", ReasonCode: "VALIDATED_BUNDLE", Message: "Local configuration, release artifacts and rendered resources validated; deployment readiness has not been measured."})
	return result, nil
}

// Configuration retains the bundle's resolved intent; a supplied source file may
// change access paths, but must not silently diverge from the generated resources.
func Configuration(b *bundle.Bundle, userConfig string) (config.Generation, error) {
	if b == nil {
		return config.Generation{}, fmt.Errorf("DPF bundle is required")
	}
	var data []byte
	for _, f := range b.Files() {
		if f.Path == bundle.ResolvedConfigPath {
			data = f.Data
		}
	}
	g, err := config.DecodeResolved(data)
	if err != nil {
		return g, err
	}
	idx := b.Index()
	if idx.Profile != g.Profile || idx.Release != g.Release || len(idx.SelectedDevices) != len(g.Devices) {
		return g, fmt.Errorf("bundle index conflicts with resolved DPF configuration")
	}
	for i, d := range g.Devices {
		if idx.SelectedDevices[i] != d.ID {
			return g, fmt.Errorf("bundle selected devices conflict with resolved DPF configuration")
		}
	}
	// A digest proves byte integrity, not that an arbitrary reindexed bundle is
	// the supported Sentry plan. Reconstruct the bounded profile contract before
	// any client is opened, using retained archives rather than local chart paths.
	if idx.Renderer != sentry.RendererVersion {
		return g, fmt.Errorf("unsupported DPF bundle renderer %q; regenerate with this binary", idx.Renderer)
	}
	archives := map[string][]byte{}
	for _, chart := range sentry.Catalog() {
		for _, f := range b.Files() {
			if f.Path == "charts/"+chart.Filename {
				archives[chart.Name] = f.Data
			}
		}
	}
	expected, err := renderBundleWithSidecar(context.Background(), g, archives, data)
	if err != nil {
		return g, fmt.Errorf("validate retained DPF intent: %w", err)
	}
	if expected.Index().BundleDigest != idx.BundleDigest {
		return g, fmt.Errorf("DPF bundle differs from its resolved profile intent; regenerate instead of editing its contents")
	}
	if userConfig == "" {
		return g, nil
	}
	in, err := config.Load(userConfig)
	if err != nil {
		return g, err
	}
	explicit, err := config.ResolveGeneration(in)
	if err != nil {
		return g, err
	}
	if !config.SameIntent(g, explicit) {
		return g, fmt.Errorf("explicit configuration changes generated DPF intent; run generate into a new bundle directory")
	}
	return explicit, nil
}
