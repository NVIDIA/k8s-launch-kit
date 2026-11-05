// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/secondarynetwork"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func secondaryNetworkWorkers(cfg *config.LaunchKitConfig) []string {
	var workers []string
	seen := map[string]bool{}
	if cfg != nil {
		for _, group := range cfg.ClusterConfig {
			for _, name := range group.WorkerNodes {
				if name != "" && !seen[name] {
					seen[name] = true
					workers = append(workers, name)
				}
			}
		}
	}
	return workers
}

func checkSecondaryNetworkDeployment(ctx context.Context, c client.Reader, ncp *unstructured.Unstructured, cfg *config.LaunchKitConfig) error {
	checks, err := secondarynetwork.CheckPolicy(ctx, c, ncp, secondaryNetworkWorkers(cfg))
	if err != nil {
		return fmt.Errorf("secondary-network ownership check: %w", err)
	}
	for _, check := range checks {
		if check.Failed || check.Progressing {
			return fmt.Errorf("external secondary-network prerequisite: %s; repair or wait for the platform installer before deploying", check.Reason)
		}
		if check.Skipped {
			ui.FromContext(ctx).Warning("%s: %s", check.Component, check.Reason)
		} else {
			ui.FromContext(ctx).Info("%s", check.Reason)
		}
	}
	return nil
}

func secondaryNetworkValidation(ctx context.Context, c client.Reader, ncp *unstructured.Unstructured, cfg *config.LaunchKitConfig, source string, flavor ...string) ([]ValidationResult, error) {
	checks, err := secondarynetwork.CheckPolicy(ctx, c, ncp, secondaryNetworkWorkers(cfg), flavor...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []ValidationResult{{Kind: "SecondaryNetworkOwnership", Name: ncp.GetName(), SourceFile: source, State: crstate.StateError, Reason: err.Error(), Detail: err.Error()}}, nil
	}
	results := make([]ValidationResult, 0, len(checks))
	for _, check := range checks {
		r := ValidationResult{Kind: "ExternalSecondaryNetwork", Name: check.Component, SourceFile: source, Skipped: check.Skipped, Reason: check.Reason, Detail: check.Reason}
		if !check.Skipped {
			r.State = crstate.StateSuccess
			if check.Failed {
				r.State = crstate.StateError
			} else if check.Progressing {
				r.State = crstate.StateInProgress
			}
			applyLegacyFlags(&r)
		}
		results = append(results, r)
	}
	return results, nil
}

func isNicClusterPolicy(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GroupVersionKind().GroupKind() == secondarynetwork.PolicyGVK.GroupKind()
}
