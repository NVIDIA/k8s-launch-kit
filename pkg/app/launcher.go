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

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	applog "github.com/nvidia/k8s-launch-kit/pkg/log"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/plugin"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// Launcher represents the main application launcher
type Launcher struct {
	options       options.Options
	logger        logr.Logger
	plugins       map[string]plugin.Plugin
	kubeClient    client.Client
	restConfig    *rest.Config
	ui            ui.Output
	jsonOutput    *ui.JSONOutput     // non-nil only in JSON mode
	result        *ui.JSONResult     // accumulated result for JSON output
	foundProfiles []profiles.Profile // populated by executeGeneration, consumed by executeDeploy
	configAssets  assets.ConfigDir
	presetCatalog *presets.Catalog
	context       context.Context
}

// New creates a new Launcher instance with the given options
func New(opts options.Options) *Launcher {
	output, jsonOut := ui.NewOutputForFormat(opts.OutputFormat, opts.Yes)
	if opts.Quiet && opts.OutputFormat != "json" {
		output = ui.NewSilent()
	}

	l := &Launcher{
		options:    opts,
		logger:     log.Log,
		plugins:    make(map[string]plugin.Plugin),
		ui:         output,
		jsonOutput: jsonOut,
		result: &ui.JSONResult{
			Success:  true,
			Phase:    "init",
			Messages: []ui.LogEntry{},
		},
		context: context.Background(),
	}

	return l
}

// Run executes the main application logic with the 3-phase workflow
func (l *Launcher) Run() error {
	return l.RunContext(context.Background())
}

// RunContext executes the launcher with the caller's cancellation context.
// Run remains as a compatibility wrapper for library consumers.
func (l *Launcher) RunContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("launcher context must not be nil")
	}
	l.context = ctx
	if l.options.LogLevel != "" {
		if err := applog.SetLogLevel(l.options.LogLevel); err != nil {
			return fmt.Errorf("failed to set log level: %w", err)
		}
	}

	resolvedAssets, err := assets.ResolveConfigDir(l.options.ConfigDir)
	if err != nil {
		return apperrors.NewValidationError(
			"invalid --config-dir",
			err,
			"Provide an accessible directory; when present, l8k-config.yaml must be a file and presets/ a directory",
		)
	}
	l.configAssets = resolvedAssets

	catalog, err := presets.CatalogForConfigDir(resolvedAssets)
	if err != nil {
		return apperrors.NewValidationError(
			"cannot load topology presets",
			err,
			"Check the presets/ directory under --config-dir",
		)
	}
	l.presetCatalog = catalog
	l.logger.V(1).Info("Using topology preset catalog", "source", catalog.Source())

	for _, pluginName := range l.options.EnabledPlugins {
		switch pluginName {
		case networkoperatorplugin.PluginName:
			l.plugins[pluginName] = &networkoperatorplugin.NetworkOperatorPlugin{
				Groups:           l.options.Groups,
				GpuType:          l.options.GpuType,
				NodeSelector:     parseNodeSelector(l.options.NodeSelector),
				KeepNamespace:    l.options.KeepNamespace,
				CollapseNicRails: l.options.CollapseNicRails,
				PresetCatalog:    l.presetCatalog,
			}
		default:
			err := fmt.Errorf("unknown plugin: %s", pluginName)
			l.logger.Error(err, "Skipping plugin")
			return apperrors.NewValidationError("unknown plugin: "+pluginName, err, "Check --enabled-plugins value")
		}
	}

	if l.options.Kubeconfig != "" {
		k8sClient, restCfg, err := kubeclient.New(l.options.Kubeconfig)
		if err != nil {
			return apperrors.NewClusterError("failed to create k8s client", err,
				"Check that kubeconfig is valid and the cluster is reachable")
		}
		l.kubeClient = k8sClient
		l.restConfig = restCfg

		// Pass REST config to network operator plugin for pod exec during discovery
		if nop, ok := l.plugins[networkoperatorplugin.PluginName]; ok {
			nop.(*networkoperatorplugin.NetworkOperatorPlugin).RESTConfig = restCfg
		}
	}

	err = l.executeWorkflow()

	// Finalize JSON output if in JSON mode
	if l.jsonOutput != nil {
		if err != nil {
			l.result.Success = false
			errJSON, _ := json.Marshal(apperrors.StructuredFromError(err))
			l.result.Error = errJSON
		}
		if finalizeErr := l.jsonOutput.Finalize(l.result); finalizeErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to write JSON output: %v\n", finalizeErr)
		}
		// In JSON mode, Finalize() already emitted the JSON to stdout. Mark
		// failures as reported so the outer CLI boundary exits without
		// emitting a second JSON object.
		if err != nil {
			return apperrors.MarkReported(err)
		}
		return nil
	}

	return err
}

// executeWorkflow executes the main 3-phase workflow
func (l *Launcher) executeWorkflow() error {
	l.ui.Header("NVIDIA Kubernetes Launch Kit")
	l.logger.Info("Starting l8k workflow")

	configPath := ""
	if l.options.DiscoverClusterConfig {
		l.result.Phase = "discover"
		l.ui.Section("Phase 1: Cluster Discovery")
		if err := l.discoverClusterConfig(); err != nil {
			l.ui.Error("Cluster discovery failed: %v", err)
			var se *apperrors.StructuredError
			if errors.As(err, &se) {
				return se // Already has context-specific suggestion
			}
			return apperrors.NewClusterError("cluster discovery failed", err,
				"Check that kubeconfig is valid and the cluster is reachable")
		}

		configPath = l.options.SaveClusterConfig
	} else {
		configPath = l.options.UserConfig
		if configPath == "" && l.options.ConfigDir != "" {
			configPath = l.configAssets.DefaultConfigPath
		}
	}

	if !l.options.DiscoverOnly {
		if err := l.executeGeneration(configPath); err != nil {
			return err
		}
	}

	if l.options.Deploy {
		if err := l.executeDeploy(); err != nil {
			return err
		}
	}

	l.ui.Success("Workflow completed successfully")
	l.logger.Info("l8k workflow completed successfully")
	return nil
}

// parseNodeSelector parses a comma-separated "key=value" string into a map.
func parseNodeSelector(s string) map[string]string {
	if s == "" {
		return nil
	}
	result := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
