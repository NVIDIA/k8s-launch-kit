// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/chartfetch"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// GenerateRequest contains only generation inputs; access references in the
// site file remain unopened. Charts are downloaded and verified before publish.
type GenerateRequest struct {
	Inventory  string
	UserConfig string
	UseCase    *string
	OutputPath string
	Deploy     bool
	Stdout     io.Writer
	Stderr     io.Writer
}

type generateDriver struct {
	request    GenerateRequest
	fetchChart func(context.Context, string) ([]byte, error)
}

func NewGenerateAdapter(request GenerateRequest) target.Driver {
	if request.UseCase != nil {
		value := *request.UseCase
		request.UseCase = &value
	}
	return generateDriver{request: request, fetchChart: (chartfetch.Downloader{}).Fetch}
}
func (d generateDriver) Descriptor() target.Descriptor { return Descriptor() }
func (d generateDriver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Generate {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if d.request.Deploy {
		return nil, apperrors.NewValidationError(
			"DPF generate --deploy is unsupported",
			nil,
			"Generate and review the bundle first, then use 'l8k deploy --target dpf' with --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp",
		)
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF generate writes a local bundle; --dry-run belongs to the deployment workflow")
	}
	if d.request.Inventory == "" || d.request.OutputPath == "" {
		return nil, fmt.Errorf("DPF generate requires --inventory dpf-discovered.yaml and --save-deployment-files ./hbn-bgp; --user-config is optional")
	}
	r := d.request
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	return target.OperationFunc(func(parent context.Context) error {
		ctx, stop := signal.NotifyContext(parent, terminationSignals()...)
		defer stop()
		progress := ui.NewSilent()
		if !inv.Output.Quiet {
			progress = ui.NewWithWriter(r.Stderr)
		}
		generated, err := app.GenerateBundle(ui.WithOutput(ctx, progress), app.GenerationRequest{
			Inventory: r.Inventory, UserConfig: r.UserConfig, UseCase: r.UseCase, OutputPath: r.OutputPath,
		}, d.fetchChart)
		if err != nil {
			return preparationError(err)
		}
		publication := generated.Publication
		result := struct {
			Target              string   `json:"target"`
			UseCase             string   `json:"useCase"`
			State               string   `json:"state"`
			Path                string   `json:"path"`
			Unchanged           bool     `json:"unchanged"`
			DeploymentReadiness string   `json:"deploymentReadiness"`
			RequiredChecks      []string `json:"requiredChecks"`
		}{Target: "dpf", UseCase: "hbn-bgp", State: "generated", Path: publication.Path, Unchanged: publication.Unchanged, DeploymentReadiness: "not-evaluated", RequiredChecks: generated.RequiredChecks}
		if inv.Output.Format == "json" {
			return json.NewEncoder(r.Stdout).Encode(result)
		}
		_, err = fmt.Fprintf(r.Stdout, "HBN-BGP manifests: %s\nDeployment readiness: not evaluated by generation; deployment requires fresh prerequisite checks.\n", publication.Path)
		return err
	}), nil
}
