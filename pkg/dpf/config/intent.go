// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
)

// Intent is desired configuration shared by discovery and generation. Resolving
// it never reads credentials, trust files, kubeconfig, or remote state.
type Intent struct {
	UseCase    string
	Namespace  string
	Release    releases.Release
	Networking *hbn.Profile
	Generation *hbn.Generation
}

func ResolveIntent(site Site) (Intent, error) {
	result := Intent{}
	release := ""
	if site.DPF.UseCase == nil {
		if site.DPF.HBN != nil || len(site.Discovery.Switches) > 0 || site.Provisioning != nil && (site.Provisioning.HBN != nil || site.Provisioning.Bootstrap != nil || site.Provisioning.Flavor != nil || site.Provisioning.Resources != nil) {
			return Intent{}, fmt.Errorf("HBN options and discovery.switches require dpf.useCase: hbn-bgp")
		}
	} else {
		if *site.DPF.UseCase != "hbn-bgp" {
			return Intent{}, fmt.Errorf("/dpf/useCase must be hbn-bgp")
		}
		definition, err := hbn.LoadDefinition()
		if err != nil {
			return Intent{}, err
		}
		result.UseCase, result.Namespace = *site.DPF.UseCase, definition.Defaults.Namespace
		release = definition.Defaults.Release
		options := hbn.Options{}
		if site.DPF.HBN != nil {
			options = *site.DPF.HBN
		}
		profile, err := hbn.Resolve(options)
		if err != nil {
			return Intent{}, fmt.Errorf("/dpf/hbn: %w", err)
		}
		generationOptions := hbn.GenerationOptions{}
		if site.Provisioning != nil && site.Provisioning.HBN != nil {
			generationOptions = *site.Provisioning.HBN
		}
		generation, err := hbn.ResolveGeneration(generationOptions)
		if err != nil {
			return Intent{}, fmt.Errorf("/provisioning/hbn: %w", err)
		}
		if err := hbn.ValidateGeneration(profile, generation); err != nil {
			return Intent{}, fmt.Errorf("/provisioning/hbn: %w", err)
		}
		result.Networking, result.Generation = &profile, &generation
	}
	if site.DPF.Namespace != nil {
		result.Namespace = *site.DPF.Namespace
		if result.Namespace == "" || len(result.Namespace) > 63 || !dnsLabel(result.Namespace) {
			return Intent{}, fmt.Errorf("/dpf/namespace must be a DNS label of at most 63 characters")
		}
	}
	if site.DPF.Release != nil {
		if *site.DPF.Release == "" {
			return Intent{}, fmt.Errorf("/dpf/release must not be empty")
		}
		release = *site.DPF.Release
	}
	resolved, err := releases.Resolve(release)
	if err != nil {
		return Intent{}, err
	}
	result.Release = resolved
	return result, nil
}
