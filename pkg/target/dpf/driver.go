// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package dpf binds the independent DPU-plane discovery CLI to typed services.
package dpf

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/switchprobe"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Descriptor advertises implemented phases; deployment remains independently gated.
func Descriptor() target.Descriptor {
	phases := map[target.Phase]target.Capability{}
	for _, p := range append(target.PublicPhases(), target.Pipeline) {
		phases[p] = target.Capability{Available: false, Reason: "DPF provisioning is not implemented in this build"}
	}
	phases[target.Generate] = target.Capability{Available: true, Reason: "HBN-BGP artifact generation; deployment readiness requires fresh checks"}
	phases[target.Discover] = target.Capability{Available: true, Reason: "experimental inventory discovery; management probes require qualified release assets; hardware catalog qualification pending"}
	return target.Descriptor{Name: target.DPF, Description: "Independent DPF discovery and HBN-BGP generation", Phases: phases}
}

type Request struct {
	UserConfig     string
	UseCase        *string
	OutputPath     string
	ExplicitOutput bool
	Stdout         io.Writer
	Stderr         io.Writer
}
type driver struct{ request Request }

func NewDiscoverAdapter(r Request) target.Driver { return driver{request: r} }
func (d driver) Descriptor() target.Descriptor   { return Descriptor() }
func (d driver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Discover {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF discovery uses discovery.probes.mode: inventory to omit probes")
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
		ctx = ui.WithOutput(ctx, progress)
		logger := log.FromContext(ctx).WithName("dpf-discovery")
		ctx = log.IntoContext(ctx, logger)
		report := discovery.NewReport()
		var store *artifacts.Store
		fail := func(code int, message string) {
			report.Execution = "failed"
			report.Errors = append(report.Errors, inventory.Problem{Code: "INVALID_CONFIGURATION", Message: message, ExitCode: code})
			progress.Info("[FAIL] Discovery preparation: %s", displayText(message))
		}
		progress.Info("[RUNNING] Configuration: validating site inputs and access settings")
		input, err := config.Load(r.UserConfig)
		if err == nil {
			input, err = config.WithUseCase(input, r.UseCase)
		}
		if err != nil {
			fail(2, err.Error())
		} else {
			resolved, err := config.Resolve(input)
			if err != nil {
				fail(2, err.Error())
			} else if err = validateManagement(resolved); err != nil {
				fail(2, err.Error())
			} else {
				// Unqualified image assets reject management mode before any remote operation.
				assets := catalog.ProbeAssets()
				if resolved.Mode == "management" && assets.Validate() != nil {
					fail(2, "management probes unavailable until the pinned diagnostic image passes Kubernetes runtime qualification; explicitly select inventory mode for the read-only preview")
				} else {
					store, err = artifacts.New(input, r.OutputPath, r.ExplicitOutput)
					if err != nil {
						fail(1, err.Error())
					} else {
						progress.Info("[PASS] Configuration: %d hosts, %d DPUs; %s mode", len(input.Site.Discovery.Hosts), len(input.Site.Discovery.DPUs), resolved.Mode)
						logger.V(1).Info("Discovery configuration ready", "mode", resolved.Mode, "sources", len(resolved.Targets), "timeout", resolved.Timeout, "requestTimeout", resolved.RequestTimeout, "concurrency", resolved.MaxConcurrentBMCs)
						if !inv.Output.Quiet {
							for _, endpoint := range resolved.Targets {
								if endpoint.Access.InsecureSkipVerify() {
									fmt.Fprintf(r.Stderr, "Warning: BMC TLS certificate verification disabled for %s\n", endpoint.Endpoint)
								}
							}
						}
						deps := discovery.Dependencies{Switches: switchprobe.Collector{}, SSH: sshprobe.Collector{}, BMC: redfish.Collector{Qualifications: catalog.Hardware(), IncludeNetworkFunctionMACs: resolved.HBN != nil}, Probes: probe.Runner{Assets: assets, Journal: probe.FileJournal(filepath.Join(store.RunDir, "resources.json"))}}
						deps.HBNPermissions = newHBNPermissionObserver().observe
						if resolved.Input.Site.Inventory == nil {
							resolved.Input.Site.Inventory = store.PriorInventory
						}
						report = discovery.Collect(ctx, resolved, store.RunID(), deps)
						progress.Info("[RUNNING] Publication: saving inventory and detailed report")
						publishStarted := time.Now()
						publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
						err = store.Publish(publishCtx, &report)
						cancel()
						if err != nil {
							logger.Error(err, "Discovery publication failed")
							progress.Info("[FAIL] Publication incomplete; see the result for details")
							report.Execution = "failed"
							report.Errors = append(report.Errors, inventory.Problem{Code: "PUBLICATION_FAILED", Message: err.Error(), ExitCode: 1})
							_ = store.Report(&report)
						} else {
							progress.Info("[PASS] Publication: %s discovery artifacts saved", report.Discovery.Outcome)
						}
						logger.V(1).Info("Discovery publication finished", "outcome", report.Discovery.Outcome, "success", err == nil, "duration", time.Since(publishStarted))
					}
				}
			}
		}
		if inv.Output.Format == "json" {
			if err := writeJSONReport(r.Stdout, report); err != nil {
				logger.Error(err, "Discovery result output failed")
				fmt.Fprintln(r.Stderr, "cannot write complete DPF discovery result:", err)
				return apperrors.MarkReported(apperrors.NewGeneralError("cannot write complete DPF discovery result", nil))
			}
		} else {
			if err := textReport(r.Stdout, report, input); err != nil {
				fmt.Fprintln(r.Stderr, "cannot write DPF discovery result")
				return apperrors.MarkReported(apperrors.NewGeneralError("cannot write DPF discovery result", nil))
			}
		}
		return apperrors.NewExitStatus(discovery.ExitCode(report))
	}), nil
}

func validateManagement(r config.Request) error {
	if r.Kubeconfig == "" {
		return nil
	}
	_, err := discovery.ManagementAccess(r)
	return err
}
