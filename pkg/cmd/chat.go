// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Interactive AI-assisted networking session",
	Long: `Start an interactive chat session with an LLM for network configuration
guidance, profile selection, or troubleshooting Network Operator issues.

Supports analysis of pre-collected sosreport diagnostics and live cluster
inspection when a kubeconfig is provided.`,
	Example: `  # Interactive troubleshooting with sosreport
  l8k chat --sosreport-path ./sosreport --llm-api-key $KEY

  # Interactive profile selection with cluster context
  l8k chat --user-config cluster-config.yaml --llm-api-key $KEY

  # Troubleshooting with live cluster access
  l8k chat --kubeconfig ~/.kube/config --llm-api-key $KEY

  # Using a specific LLM model
  l8k chat --llm-api-key $KEY --llm-vendor anthropic \
    --llm-model claude-3-5-sonnet-20241022`,
	Run: func(cmd *cobra.Command, args []string) {
		opts := options.Options{
			LLMInteractive:          true,
			Kubeconfig:              kubeconfig,
			UserConfig:              userConfig,
			SosreportPath:           sosreportPath,
			LLMApiKey:               llmApiKey,
			LLMApiUrl:               llmApiUrl,
			LLMVendor:               llmVendor,
			LLMModel:                llmModel,
			LLMThrottle:             llmThrottle,
			NetworkOperatorNamespace: networkOperatorNamespace,
			NetworkOperatorRelease:   networkOperatorRelease,
			EnabledPlugins:           parseEnabledPlugins(enabledPlugins),
			OutputFormat:             outputFormat,
			Yes:                      yesFlag,
			Quiet:                    quietFlag,
		}

		// Resolve kubeconfig if provided (optional for chat)
		if kubeconfig != "" || opts.SosreportPath == "" {
			if resolved, err := resolveKubeconfig(kubeconfig); err == nil {
				opts.Kubeconfig = resolved
			}
		}

		// Validate LLM options
		if opts.LLMApiKey == "" {
			exitWithError(apperrors.NewValidationError(
				"--llm-api-key is required for chat mode",
				nil,
				"Pass --llm-api-key <key> or set the appropriate environment variable",
			), opts.OutputFormat)
		}

		launcher := app.New(opts)
		if err := launcher.Run(); err != nil {
			var se *apperrors.StructuredError
			if !errors.As(err, &se) {
				se = apperrors.NewGeneralError(err.Error(), err)
			}
			exitWithError(se, opts.OutputFormat)
		}
	},
}

func init() {
	rootCmd.AddCommand(chatCmd)

	chatCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (optional, falls back to $KUBECONFIG)")
	chatCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config for context")
	chatCmd.Flags().StringVar(&sosreportPath, "sosreport-path", "", "Path to sosreport directory for analysis")
	chatCmd.Flags().StringVar(&llmApiKey, "llm-api-key", "", "API key for the LLM API (required)")
	chatCmd.Flags().StringVar(&llmApiUrl, "llm-api-url", "", "API URL for the LLM API")
	chatCmd.Flags().StringVar(&llmVendor, "llm-vendor", "openai-azure", "LLM vendor: openai, openai-azure, anthropic, gemini")
	chatCmd.Flags().StringVar(&llmModel, "llm-model", "", "LLM model name")
	chatCmd.Flags().BoolVar(&llmThrottle, "llm-throttle", false, "Enable rate limit throttling")
	chatCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override operator namespace")
	chatCmd.Flags().StringVar(&networkOperatorRelease, "network-operator-release", "",
		fmt.Sprintf("Network Operator release line to deploy (MAJOR.MINOR). Supported: %s",
			strings.Join(networkoperatorplugin.SupportedReleases(), ", ")))
	chatCmd.Flags().StringVar(&enabledPlugins, "enabled-plugins", "network-operator", "Comma-separated list of plugins to enable")

	_ = chatCmd.MarkFlagRequired("llm-api-key")
}
