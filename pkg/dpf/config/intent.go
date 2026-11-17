// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"

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
	useCase := ""
	if site.DPF.UseCase == nil {
		if site.DPF.HBN != nil || len(site.Discovery.Switches) > 0 || site.Provisioning != nil && (site.Provisioning.HBN != nil || site.Provisioning.Bootstrap != nil || site.Provisioning.Flavor != nil || site.Provisioning.Resources != nil) {
			return Intent{}, fmt.Errorf("HBN options and discovery.switches require dpf.useCase: hbn-bgp")
		}
	} else {
		if *site.DPF.UseCase != "hbn-bgp" {
			return Intent{}, fmt.Errorf("/dpf/useCase must be hbn-bgp")
		}
		useCase = *site.DPF.UseCase
	}

	releaseVersion := ""
	if site.DPF.Release != nil {
		if *site.DPF.Release == "" {
			return Intent{}, fmt.Errorf("/dpf/release must not be empty")
		}
		releaseVersion = *site.DPF.Release
	} else if useCase != "" {
		defaultRelease, err := releases.DefaultForUseCase(useCase)
		if err != nil {
			return Intent{}, err
		}
		releaseVersion = defaultRelease.Version
	}
	resolved, err := releases.Resolve(releaseVersion)
	if err != nil {
		return Intent{}, err
	}
	if useCase != "" && !resolved.SupportsUseCase(useCase) {
		return Intent{}, fmt.Errorf("/dpf/release %q is unsupported for %s; supported releases: %s", resolved.Version, useCase, strings.Join(releases.SupportedUseCaseReleases(useCase), ", "))
	}
	result.UseCase, result.Release = useCase, resolved
	if useCase != "" {
		definition, err := hbn.LoadDefinition()
		if err != nil {
			return Intent{}, err
		}
		result.Namespace = definition.Defaults.Namespace
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
		if resolved.HBNWorkload == nil {
			return Intent{}, fmt.Errorf("selected DPF release %s has no HBN workload coordinates", resolved.Version)
		}
		coordinates := hbn.GenerationCoordinates{
			ChartRepository: resolved.HBNWorkload.ChartRepository, Chart: resolved.HBNWorkload.Chart,
			ChartVersion: resolved.HBNWorkload.ChartVersion, ChartDigest: resolved.HBNWorkload.ChartDigest,
			ImageRepository: resolved.HBNWorkload.ImageRepository, ImageTag: resolved.HBNWorkload.ImageTag,
			ImageDigest: resolved.HBNWorkload.ImageDigest, StartupVersion: resolved.HBNWorkload.StartupVersion,
		}
		generation, err := hbn.ResolveGenerationFrom(definition.Defaults.Generation, coordinates, generationOptions)
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
	return result, nil
}
