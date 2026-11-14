// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// GenerateRequest contains only generation inputs; access references in the
// site file remain unopened. Charts are downloaded and verified before publish.
type GenerateRequest struct {
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
	return generateDriver{request: request, fetchChart: downloadGenerationChart}
}
func (d generateDriver) Descriptor() target.Descriptor { return Descriptor() }
func (d generateDriver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Generate {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if d.request.Deploy {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: target.Deploy, Reason: Descriptor().Capability(target.Deploy).Reason}
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF generate writes a local bundle; --dry-run belongs to the deployment workflow")
	}
	if d.request.UserConfig == "" || d.request.OutputPath == "" {
		return nil, fmt.Errorf("DPF generate requires --user-config and --save-deployment-files")
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
		if err := ctx.Err(); err != nil {
			return err
		}
		progress := ui.NewSilent()
		if !inv.Output.Quiet {
			progress = ui.NewWithWriter(r.Stderr)
		}
		progress.Info("[RUNNING] Generation: resolving profile and selected discovery identities")
		input, err := config.Load(r.UserConfig)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF generation inputs", err, "Correct the site configuration and refresh discovery if selected identities changed")
		}
		input, err = config.WithUseCase(input, r.UseCase)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF generation inputs", err, "Correct the site configuration and refresh discovery if selected identities changed")
		}
		resolved, err := generationInputs(input)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF generation inputs", err, "Correct the site configuration and refresh discovery if selected identities changed")
		}
		progress.Info("[RUNNING] Charts: retrieving the selected DPF operator archive")
		resolved.OperatorArchive, err = d.fetchChart(ctx, resolved.OperatorChartURL)
		if err != nil {
			return fmt.Errorf("retrieve DPF operator chart: %w", err)
		}
		progress.Info("[RUNNING] Charts: retrieving the selected HBN archive")
		hbn := resolved.Workload.Generation
		resolved.HBNChartArchive, err = d.fetchChart(ctx, strings.TrimSuffix(hbn.ChartRepository, "/")+"/charts/"+hbn.ChartName+"-"+hbn.ChartVersion+".tgz")
		if err != nil {
			return fmt.Errorf("retrieve HBN chart: %w", err)
		}
		progress.Info("[RUNNING] Manifests: verifying archives and validating the complete resource bundle")
		artifacts, err := generation.Generate(resolved.Inputs)
		if err != nil {
			return err
		}
		publication, err := artifacts.Publish(ctx, r.OutputPath)
		if err != nil {
			return err
		}
		index := artifacts.Index()
		progress.Info("[PASS] Generation: %d stages saved to %s", len(index.Stages), publication.Path)
		result := struct {
			Target              string   `json:"target"`
			UseCase             string   `json:"useCase"`
			State               string   `json:"state"`
			Path                string   `json:"path"`
			Unchanged           bool     `json:"unchanged"`
			DeploymentReadiness string   `json:"deploymentReadiness"`
			RequiredChecks      []string `json:"requiredChecks"`
		}{Target: "dpf", UseCase: "hbn-bgp", State: "generated", Path: publication.Path, Unchanged: publication.Unchanged, DeploymentReadiness: "not-evaluated", RequiredChecks: index.ExternalPrerequisites}
		if inv.Output.Format == "json" {
			return json.NewEncoder(r.Stdout).Encode(result)
		}
		_, err = fmt.Fprintf(r.Stdout, "HBN-BGP manifests: %s\nDeployment readiness: not evaluated by generation; deployment requires fresh prerequisite checks.\n", publication.Path)
		return err
	}), nil
}

const maximumGenerationChartBytes = 32 << 20

func downloadGenerationChart(ctx context.Context, address string) ([]byte, error) {
	return downloadChart(ctx, address, http.DefaultTransport)
}

// Inputs remain unsigned public coordinates. NGC redirects these coordinates to
// signed HTTPS object URLs; queries are allowed only on those server redirects.
// Integrity is verified against the selected digest before bundle publication.
func downloadChart(ctx context.Context, address string, transport http.RoundTripper) ([]byte, error) {
	validate := func(address string, redirected bool) error {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (!redirected && parsed.RawQuery != "") || parsed.Fragment != "" {
			return fmt.Errorf("chart URL requires HTTPS without user information or fragment; input coordinates must not contain a query")
		}
		return nil
	}
	if err := validate(address, false); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot construct chart download request")
	}
	unsafeRedirect := errors.New("chart redirect requires HTTPS without user information or fragment")
	redirectLimit := errors.New("chart redirect limit exceeded")
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return redirectLimit
		}
		if err := validate(req.URL.String(), true); err != nil {
			return unsafeRedirect
		}
		// Do not propagate a prior signed object URL as a Referer on another hop.
		req.Header.Del("Referer")
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		// net/http wraps failures with the final URL, which can contain a signed
		// credential. Never return that wrapper or arbitrary server text.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, unsafeRedirect) {
			return nil, unsafeRedirect
		}
		if errors.Is(err, redirectLimit) {
			return nil, redirectLimit
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, fmt.Errorf("chart download timed out")
		}
		return nil, fmt.Errorf("chart download failed; verify HTTPS connectivity and CA trust")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chart server returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumGenerationChartBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot read complete chart archive")
	}
	if len(data) == 0 || len(data) > maximumGenerationChartBytes {
		return nil, fmt.Errorf("chart archive must be between 1 byte and %d bytes", maximumGenerationChartBytes)
	}
	return data, nil
}
