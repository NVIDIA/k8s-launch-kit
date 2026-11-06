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
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

type WorkflowRequest struct {
	Phase           target.Phase
	UserConfig      string
	OutputPath      string
	DeploymentFiles string
	Overrides       configinput.Values
	Deploy          bool
	Wait            time.Duration
	Stdout          io.Writer
	Stderr          io.Writer
}

type workflowDriver struct{ request WorkflowRequest }

func NewWorkflowAdapter(r WorkflowRequest) target.Driver {
	r.Overrides = r.Overrides.Clone()
	return workflowDriver{request: r}
}

func (workflowDriver) Descriptor() target.Descriptor { return Descriptor() }

type workflowResult struct {
	ExitCode   int                `json:"exitCode"`
	Target     string             `json:"target"`
	Phase      target.Phase       `json:"phase"`
	Outcome    string             `json:"outcome"`
	Generation *generation.Result `json:"generation,omitempty"`
	Deployment *deployment.Result `json:"deployment,omitempty"`
	Errors     []string           `json:"errors,omitempty"`
}

func (d workflowDriver) Bind(inv target.Invocation) (target.Operation, error) {
	r := d.request
	if inv.Phase != r.Phase || (inv.Phase != target.Generate && inv.Phase != target.Deploy && inv.Phase != target.Validate) {
		return nil, fmt.Errorf("DPF workflow adapter does not bind phase %q", inv.Phase)
	}
	if r.Wait < 0 {
		return nil, fmt.Errorf("validation wait must not be negative")
	}
	if inv.Phase == target.Generate && inv.Execution.DryRun && !r.Deploy {
		return nil, fmt.Errorf("DPF generate --dry-run requires --deploy; generation alone is local and does not mutate a cluster")
	}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	return target.OperationFunc(func(parent context.Context) error {
		ctx, stop := signal.NotifyContext(parent, terminationSignals()...)
		defer stop()
		timeout := inv.Execution.Timeout
		if inv.Phase == target.Validate {
			timeout = r.Wait
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		result := workflowResult{Target: "dpf", Phase: inv.Phase, Outcome: "failed"}
		var runErr error
		exitCode := 1
		path := r.DeploymentFiles
		if inv.Phase == target.Generate {
			generated, err := generation.Run(ctx, generation.Request{UserConfig: r.UserConfig, OutputPath: r.OutputPath, Overrides: r.Overrides})
			result.Generation = &generated
			runErr = err
			if err != nil && (len(generated.Checks) == 0 || generated.Checks[len(generated.Checks)-1].ID != "publication") {
				exitCode = 2
			}
			path = generated.BundlePath
		}
		if runErr == nil && (inv.Phase != target.Generate || r.Deploy) {
			var b *bundle.Bundle
			b, runErr = bundle.Load(path)
			if runErr != nil {
				exitCode = 2
			}
			if runErr == nil {
				// Generate already captured overrides in the resolved snapshot; source
				// must not be reapplied over that snapshot during --deploy.
				source := r.UserConfig
				if inv.Phase == target.Generate {
					source = ""
				}
				g, err := generation.Configuration(b, source)
				runErr = err
				if err != nil {
					exitCode = 2
				}
				if runErr == nil {
					mode := deployment.Deploy
					if inv.Phase == target.Validate {
						mode = deployment.Validate
					} else if inv.Execution.DryRun {
						mode = deployment.DryRun
					}
					absolute, err := filepath.Abs(path)
					runErr = err
					if runErr == nil {
						deployed, err := (deployment.Runner{EvidenceDir: absolute + ".runs", Timeout: timeout, Wait: r.Wait > 0}).Run(ctx, b, g, mode)
						result.Deployment = &deployed
						runErr = err
					}
				}
			}
		}
		if runErr != nil {
			result.ExitCode = exitCode
			result.Errors = append(result.Errors, runErr.Error())
		} else {
			result.Outcome = "complete"
		}
		if inv.Output.Format == "json" {
			if err := json.NewEncoder(r.Stdout).Encode(result); err != nil {
				return apperrors.MarkReported(apperrors.NewGeneralError("cannot encode DPF workflow result", err))
			}
		} else {
			fmt.Fprintf(r.Stdout, "DPF %s: %s\n", inv.Phase, result.Outcome)
			if result.Generation != nil && result.Generation.BundlePath != "" {
				fmt.Fprintf(r.Stdout, "Bundle: %s\n", result.Generation.BundlePath)
			}
			if result.Deployment != nil {
				for _, check := range result.Deployment.Checks {
					if check.State != "passed" {
						fmt.Fprintf(r.Stdout, "%s %s: %s (%s): %s\n", check.ID, check.Subject, check.State, check.ReasonCode, check.Message)
					}
				}
				if result.Deployment.ReportPath != "" {
					fmt.Fprintf(r.Stdout, "Report: %s\n", result.Deployment.ReportPath)
				}
			}
			for _, message := range result.Errors {
				fmt.Fprintln(r.Stderr, message)
			}
		}
		if runErr != nil {
			return apperrors.NewExitStatus(exitCode)
		}
		return nil
	}), nil
}
