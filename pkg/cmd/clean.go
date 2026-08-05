// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

var keepHelmChart bool

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove a Network Operator deployment from a Kubernetes cluster",
	Long: `Delete custom resources associated with the Network Operator deployment and,
by default, uninstall the network-operator Helm release.

The operator namespace is resolved from --network-operator-namespace, then a
user or explicit default config, and finally defaults to
nvidia-network-operator. All namespaced custom resources in that namespace are
deleted. The known cluster-scoped Network Operator policy and network custom
resources are also removed. Custom installation namespaces must be supplied by
flag or config; untrusted in-cluster objects never select the cleanup target.

The namespace and CustomResourceDefinitions are preserved. Pass
--keep-helm-chart to remove the custom resources while leaving the Helm release
and its chart-managed resources installed.`,
	Example: `  # Remove Network Operator CRs and uninstall the Helm release
  l8k clean --kubeconfig ~/.kube/config

  # Resolve a custom operator namespace explicitly
  l8k clean --kubeconfig ~/.kube/config \
    --network-operator-namespace custom-network-operator

  # Remove all CRs but keep the operator chart installed
  l8k clean --kubeconfig ~/.kube/config --keep-helm-chart

  # Non-interactive agent mode (JSON output auto-confirms)
  l8k clean --kubeconfig ~/.kube/config --output json`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"kubeconfig required for clean",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			), outputFormat)
		}

		k8sClient, restConfig, err := kubeclient.New(resolved)
		if err != nil {
			exitWithError(apperrors.NewClusterError(
				"failed to create Kubernetes client",
				err,
				"Check that kubeconfig is valid and the cluster is reachable",
			), outputFormat)
		}

		uiOutput, jsonOutput := ui.NewOutputForFormat(outputFormat, yesFlag)
		ctx := ui.WithOutput(cmd.Context(), uiOutput)

		namespace, source, err := resolveCleanNamespace()
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"failed to resolve namespace from user config",
				err,
				"Fix the config or pass --network-operator-namespace <namespace>",
			), outputFormat)
		}
		if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
			exitWithError(apperrors.NewValidationError(
				fmt.Sprintf("invalid Network Operator namespace %q", namespace),
				fmt.Errorf("%s", problems[0]),
				"Pass a valid DNS-1123 namespace with --network-operator-namespace",
			), outputFormat)
		}

		uiOutput.Section("Network Operator cleanup")
		uiOutput.Info("Target namespace: %s (%s)", namespace, source)
		if keepHelmChart {
			uiOutput.Info("Helm release: keep %s installed", "network-operator")
		} else {
			uiOutput.Info("Helm release: uninstall %s", "network-operator")
		}

		confirmed, err := uiOutput.Confirm(fmt.Sprintf(
			"Delete all custom resources in namespace %s and the known cluster-scoped Network Operator resources?",
			namespace,
		))
		if err != nil {
			exitWithError(apperrors.NewGeneralError("failed to read cleanup confirmation", err), outputFormat)
		}
		if !confirmed {
			uiOutput.Info("Cleanup cancelled; no changes were made")
			finalizeCleanJSON(jsonOutput, namespace, 0, false, keepHelmChart)
			return
		}

		result, err := networkoperatorplugin.Clean(ctx, k8sClient, networkoperatorplugin.CleanOptions{
			Namespace:     namespace,
			KeepHelmChart: keepHelmChart,
			RestConfig:    restConfig,
		})
		if err != nil {
			var structured *apperrors.StructuredError
			if errors.As(err, &structured) {
				exitWithError(structured, outputFormat)
			}
			exitWithError(apperrors.NewDeploymentError(
				"Network Operator cleanup failed",
				err,
				"Resolve any stuck finalizers or RBAC errors, then re-run `l8k clean`",
			), outputFormat)
		}

		uiOutput.Success("Cleanup completed: deleted %d custom resource(s)", result.CustomResourcesDeleted)
		finalizeCleanJSON(
			jsonOutput,
			result.Namespace,
			result.CustomResourcesDeleted,
			result.HelmReleaseRemoved,
			keepHelmChart,
		)
	},
}

func resolveCleanNamespace() (string, string, error) {
	if networkOperatorNamespace != "" {
		return networkOperatorNamespace, "--network-operator-namespace", nil
	}
	// Cleanup only needs one field from the user config. Do not apply release
	// catalog defaults or validate unrelated deployment settings here: an old
	// generated config must not prevent the operator from being removed.
	path := userConfigPathBeforeDefaults()
	if path == "" && configDir != "" {
		var err error
		path, err = defaultConfigPathFor(configDir)
		if err != nil {
			return "", "--config-dir", err
		}
	}
	if path == "" {
		return defaultOperatorNamespace, "default", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", path, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg struct {
		NetworkOperator *struct {
			Namespace string `yaml:"namespace"`
		} `yaml:"networkOperator"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.NetworkOperator == nil || cfg.NetworkOperator.Namespace == "" {
		return defaultOperatorNamespace, "default", nil
	}
	return cfg.NetworkOperator.Namespace, path, nil
}

func finalizeCleanJSON(
	jsonOutput *ui.JSONOutput,
	namespace string,
	resources int,
	helmRemoved bool,
	keepHelm bool,
) {
	if jsonOutput == nil {
		return
	}
	_ = jsonOutput.Finalize(&ui.JSONResult{
		Success:  true,
		Phase:    "clean",
		Deployed: false,
		Cleanup: &ui.CleanupResult{
			Namespace:              namespace,
			CustomResourcesDeleted: resources,
			HelmReleaseRemoved:     helmRemoved,
			KeepHelmChart:          keepHelm,
		},
	})
}

func init() {
	rootCmd.AddCommand(cleanCmd)

	cleanCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG, then ~/.kube/config)")
	cleanCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file used to resolve networkOperator.namespace")
	cleanCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override the Network Operator namespace from cluster-config.yaml")
	cleanCmd.Flags().BoolVar(&keepHelmChart, "keep-helm-chart", false, "Delete Network Operator custom resources but keep the network-operator Helm release installed")

	setFlagGroup(cleanCmd, "kubeconfig", GroupCommon)
	setFlagGroup(cleanCmd, "user-config", GroupCommon)
	setFlagGroup(cleanCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(cleanCmd, "keep-helm-chart", GroupClean)
}
