// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
)

func (l *Launcher) assessNVAIE(ctx context.Context, cfg *config.LaunchKitConfig) *nvaie.Assessment {
	workers := []string{}
	products := map[string]string{}
	for _, group := range cfg.ClusterConfig {
		for _, name := range group.WorkerNodes {
			workers = append(workers, name)
			products[name] = group.GPUType
		}
	}
	if l.assessNVAIEFunc != nil {
		return l.assessNVAIEFunc(ctx, l.restConfig, cfg.NVAIERelease, workers, products, discoveryDOCAHost(cfg))
	}
	return assessNVAIE(ctx, l.restConfig, cfg.NVAIERelease, workers, products, discoveryDOCAHost(cfg))
}

func assessNVAIE(ctx context.Context, restConfig *rest.Config, release string, workers []string, products map[string]string, docaHost *nvaie.DOCAHostEvidence) *nvaie.Assessment {
	if restConfig == nil {
		return unavailableNVAIE(release, "Kubernetes client configuration unavailable")
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return unavailableNVAIE(release, "Kubernetes client initialization failed: "+err.Error())
	}
	return nvaie.Assess(ctx, client, release, workers, products, docaHost)
}

func unavailableNVAIE(release, detail string) *nvaie.Assessment {
	return &nvaie.Assessment{Release: release, Checks: []nvaie.Check{{Name: "live observation", Status: nvaie.Unknown, Required: true, Detail: detail}}}
}

func discoveryDOCAHost(cfg *config.LaunchKitConfig) *nvaie.DOCAHostEvidence {
	if cfg == nil {
		return nil
	}
	installed, _, _ := config.DOCAHostCoverage(cfg)
	if installed == 0 && (cfg.DOCADriver == nil || cfg.DOCADriver.Enable) {
		return nil
	}
	evidence := &nvaie.DOCAHostEvidence{MembershipKnown: config.DOCAHostScopeForConfig(cfg).MembershipKnown, Fresh: true}
	for _, observation := range config.SelectedDOCAHost(cfg) {
		evidence.Observations = append(evidence.Observations, nvaie.HostPackageObservation{Node: observation.Node, Status: observation.Status, Version: observation.Version, Reason: observation.Reason})
	}
	return evidence
}
