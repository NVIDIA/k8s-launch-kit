// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func validateCleanPlatform(openShift bool, requested, files string) error {
	switch {
	case requested != "" && requested != config.FlavorK8s && requested != config.FlavorOCP:
		return fmt.Errorf("unknown cleanup flavor %q", requested)
	case openShift && requested == config.FlavorK8s:
		return fmt.Errorf("cannot run Kubernetes cleanup on an OpenShift cluster")
	case !openShift && requested == config.FlavorOCP:
		return fmt.Errorf("OpenShift cleanup was selected but the cluster does not serve the OpenShift ClusterVersion CRD")
	case !openShift && files != "":
		return fmt.Errorf("--deployment-files selects an OpenShift configuration; the target cluster is Kubernetes")
	}
	return nil
}

// Load both inputs from the same deployment. Never select deletion targets by
// re-rendering profiles, consulting an installed catalog, or scanning namespaces.
func loadOCPCleanBundle(dir string) (*config.LaunchKitConfig, *bundle.Bundle, error) {
	if dir == "" {
		dir = DefaultDeploymentDir
	}
	root := filepath.Clean(dir)
	if filepath.Base(root) == "network-operator" {
		// A deployment root may itself be called network-operator.
		if _, err := os.Stat(config.EffectiveConfigPath(root)); os.IsNotExist(err) {
			root = filepath.Dir(root)
		}
	}
	cfg, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(root))
	if err != nil {
		return nil, nil, fmt.Errorf("load retained OpenShift configuration (pass --deployment-files with the generated deployment directory): %w", err)
	}
	if cfg.Flavor != config.FlavorOCP || cfg.NetworkOperator == nil {
		return nil, nil, fmt.Errorf("expected an OpenShift Host bundle with a networkOperator configuration in %s", root)
	}
	artifacts, err := bundle.Load(os.DirFS(filepath.Join(root, "network-operator")))
	if err != nil {
		return nil, nil, fmt.Errorf("load OpenShift deployment manifests from %s: %w", root, err)
	}
	return cfg, artifacts, nil
}

// An explicit user config or namespace can confirm the retained bundle's
// target, but cannot redirect any of its namespaced or cluster-scoped CRs.
func checkOCPCleanOverrides(path, namespace, override string) error {
	if problems := validation.IsDNS1123Label(namespace); len(problems) != 0 {
		return fmt.Errorf("invalid operator namespace %q in retained bundle: %s", namespace, problems[0])
	}
	if override != "" && override != namespace {
		return fmt.Errorf("--network-operator-namespace %q differs from retained bundle namespace %q", override, namespace)
	}
	if path == "" {
		return nil
	}
	var supplied struct {
		Flavor          string `json:"flavor"`
		NetworkOperator struct {
			Namespace string `json:"namespace"`
		} `json:"networkOperator"`
	}
	if config.IsEffectiveConfigPath(path) {
		cfg, err := config.LoadEffectiveConfig(path)
		if err != nil {
			return err
		}
		supplied.Flavor = cfg.Flavor
		if cfg.NetworkOperator != nil {
			supplied.NetworkOperator.Namespace = cfg.NetworkOperator.Namespace
		}
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &supplied); err != nil {
			return fmt.Errorf("read --user-config: %w", err)
		}
	}
	if supplied.Flavor != "" && supplied.Flavor != config.FlavorOCP {
		return fmt.Errorf("--user-config selects flavor %q for an OpenShift target", supplied.Flavor)
	}
	if op := supplied.NetworkOperator; op.Namespace != "" && op.Namespace != namespace {
		return fmt.Errorf("--user-config specifies operator namespace %q, retained bundle specifies %q", op.Namespace, namespace)
	}
	return nil
}

func runOpenShiftClean(ctx context.Context, c client.Client, restConfig *rest.Config, out ui.Output, jsonOutput *ui.JSONOutput) error {
	cfg, artifacts, err := loadOCPCleanBundle(cleanDeploymentFiles)
	if err != nil {
		return err
	}
	ns := cfg.NetworkOperator.Namespace
	if ns == "" {
		ns = defaultOperatorNamespace
	}
	if err := checkOCPCleanOverrides(userConfig, ns, networkOperatorNamespace); err != nil {
		return err
	}
	plan, err := networkoperatorplugin.PrepareOpenShiftClean(ctx, c, cfg, artifacts)
	if err != nil {
		return err
	}
	out.Section("OpenShift configuration cleanup")
	out.Info("Network Operator namespace: %s. OLM installations will be retained.", ns)
	for _, target := range plan.Targets() {
		out.Info("%s", target)
	}
	confirmed, err := out.Confirm("Remove the listed deployment configuration and restore recorded changes to shared OpenShift settings?")
	if err != nil {
		return err
	}
	if !confirmed {
		out.Info("Cleanup cancelled")
		finalizeCleanJSON(jsonOutput, ns, 0, false, true)
		return nil
	}
	result, err := plan.Execute(ctx, networkoperatorplugin.CleanOptions{
		Namespace: ns, RestConfig: restConfig, SkipWait: skipCleanWait, ResourceTimeout: cleanTimeout,
	})
	if err != nil {
		return err
	}
	for _, effect := range result.Retained {
		out.Warning("%s", effect)
	}
	out.Success("OpenShift cleanup completed: deleted %d custom resource(s)", result.CustomResourcesDeleted)
	finalizeCleanJSON(jsonOutput, ns, result.CustomResourcesDeleted, false, true, result.Retained...)
	return nil
}
