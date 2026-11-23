// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func pendingNVAIE(release, detail string) *nvaie.Assessment {
	if release == "" {
		return nil
	}
	return &nvaie.Assessment{Release: release, Checks: []nvaie.Check{{Name: "live observation", Status: nvaie.Unknown, Required: true, Detail: detail}}}
}

// requestedNVAIE retains the requested check even if validation stops before
// kubeconfig, artifact or normal config initialization can complete.
func requestedNVAIE(request ValidateRequest) *nvaie.Assessment {
	release := request.NVAIERelease.Value
	if !request.NVAIERelease.Set {
		path, err := UserConfigPathFor(UserConfigInput{Explicit: request.UserConfig, DeploymentFiles: request.DeploymentFiles, ConfigDir: request.ConfigDir})
		if err == nil && path != "" {
			if config.IsEffectiveConfigPath(path) {
				cfg, loadErr := config.LoadEffectiveConfig(path)
				if loadErr == nil {
					release = cfg.NVAIERelease
				}
			} else {
				input, loadErr := config.LoadInput(path, log.Log)
				if loadErr == nil {
					release = input.Config.NVAIERelease
				}
			}
		}
	}
	return pendingNVAIE(release, "Validation stopped before live NVAIE observations completed")
}

func assessNVAIE(ctx context.Context, restConfig *rest.Config, release string, workers []string, docaHost *nvaie.DOCAHostEvidence) *nvaie.Assessment {
	if restConfig == nil {
		return pendingNVAIE(release, "Kubernetes client configuration unavailable")
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return pendingNVAIE(release, "Kubernetes client initialization failed: "+err.Error())
	}
	// Saved inventory GPU products deliberately are not live validation evidence.
	return nvaie.Assess(ctx, client, release, workers, nil, docaHost)
}

func nvaieVerdict(overall connectivity.OverallVerdict, assessment *nvaie.Assessment) connectivity.OverallVerdict {
	if assessment != nil && !assessment.Passed() {
		overall.Pass = false
		overall.Reasons = append(overall.Reasons, fmt.Sprintf("NVAIE %s matrix/component checks failed or remain unknown", assessment.Release))
	}
	return overall
}

func emitNVAIE(assessment *nvaie.Assessment, format string) {
	if assessment == nil {
		return
	}
	if format == "json" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"nvaie": assessment})
		return
	}
	status := "PASSED"
	if !assessment.Passed() {
		status = "FAILED / UNKNOWN"
	}
	fmt.Fprintf(os.Stderr, "\nNVAIE %s matrix/component checks: %s\n", assessment.Release, status)
	for _, check := range assessment.Checks {
		fmt.Fprintf(os.Stderr, "  %s: %s — %s\n", check.Name, check.Status, check.Detail)
	}
}

// savedDOCAHost uses the preserved discovery membership, not preset render nodes.
func savedDOCAHost(cfg *config.LaunchKitConfig) *nvaie.DOCAHostEvidence {
	if cfg == nil {
		return nil
	}
	evidence := &nvaie.DOCAHostEvidence{MembershipKnown: config.DOCAHostScopeForConfig(cfg).MembershipKnown, Fresh: false}
	for _, observation := range config.SelectedDOCAHost(cfg) {
		evidence.Observations = append(evidence.Observations, nvaie.HostPackageObservation{Node: observation.Node, Status: observation.Status, Version: observation.Version, Reason: observation.Reason})
	}
	return evidence
}
