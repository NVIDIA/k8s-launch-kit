// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// Request contains the immutable inputs to one resolution run.
type Request struct {
	Input    *config.Input
	Defaults *config.Input
	Options  options.Options

	// ApplyHardwareDefaults enables defaults derived from the selected
	// clusterConfig. Generate and fresh discovery set this; callers that are
	// intentionally preserving a partial config can leave it false.
	ApplyHardwareDefaults bool
	// ValidateReady enables profile cohort validation required before render.
	ValidateReady bool
}

// Result contains the effective config and an audit trail of hardware-derived
// decisions. The input objects are never mutated.
type Result struct {
	Config    *config.LaunchKitConfig
	Decisions []DefaultDecision
}

// Resolve applies the single precedence pipeline:
//
//	canonical defaults < hardware defaults < user YAML < explicit CLI
//
// User YAML starts as the target object, hardware defaults fill only missing
// profile values, CLI mappings overlay explicit values, and canonical defaults
// fill every remaining omitted field. Catalog expansion and validation operate
// on the resulting effective config.
func Resolve(request Request) (*Result, error) {
	if request.Input == nil || request.Input.Config == nil {
		return nil, fmt.Errorf("resolver input must not be nil")
	}
	defaults := request.Defaults
	if defaults == nil {
		var err error
		defaults, err = config.DefaultInput()
		if err != nil {
			return nil, fmt.Errorf("load canonical defaults: %w", err)
		}
	}
	if defaults.Config == nil {
		return nil, fmt.Errorf("resolver defaults must not be nil")
	}

	effective, err := config.CloneConfig(request.Input.Config)
	if err != nil {
		return nil, err
	}
	present := clonePathSet(request.Input.Present)
	inputs := configflags.Infer(request.Options)
	for _, override := range inputs.Overrides {
		present.Add(override.Path)
	}
	for _, complex := range inputs.Requests {
		for _, path := range configflags.ConfigPathsForFlag(complex.Flag) {
			present.Add(path)
		}
	}

	var decisions []DefaultDecision
	if request.ApplyHardwareDefaults {
		decisions = ApplyHardwareDefaultsWithPresence(effective, request.Options, present)
	}
	if err := ApplyExplicitConfigInputs(inputs, effective); err != nil {
		return nil, err
	}
	if err := config.MergeDefaults(effective, defaults.Config, present); err != nil {
		return nil, fmt.Errorf("merge canonical defaults: %w", err)
	}

	// selectedRelease is authoritative regardless of which layer selected it.
	// Catalog expansion deliberately runs after all layers are merged so stale
	// hand-written version/repository fields cannot override the cohort.
	selectedRelease := ""
	if effective.NetworkOperator != nil {
		selectedRelease = effective.NetworkOperator.SelectedRelease
	}
	if err := ExpandNetworkOperatorRelease(selectedRelease, effective); err != nil {
		return nil, fmt.Errorf("expand networkOperator.selectedRelease: %w", err)
	}
	if err := config.NormalizeResolvedConfigWithPresence(effective, request.Input.SourcePath, present); err != nil {
		return nil, err
	}
	if request.ValidateReady {
		if err := ValidateResolvedConfig(effective); err != nil {
			return nil, err
		}
	}

	return &Result{Config: effective, Decisions: decisions}, nil
}

func clonePathSet(source config.PathSet) config.PathSet {
	clone := config.PathSet{}
	for path := range source {
		clone.Add(path)
	}
	return clone
}
