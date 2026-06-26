// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// userconfig.go centralises how `l8k` subcommands locate and load the
// user-supplied cluster-config.yaml. Both `l8k validate` and `l8k deploy`
// read the same file to drive their release-aware checks; keeping the
// resolution + loading here means the two stay in lock-step when the
// auto-discovery rules change.

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// defaultUserConfigPath is the conventional location `l8k discover` writes
// its output to and where subcommands look first when --user-config is
// not supplied.
const defaultUserConfigPath = "./cluster-config.yaml"

// userConfigPath returns the user-config file path to read, or "" when
// none is found. Lookup order (first hit wins):
//
//  1. The explicit --user-config when set (mandatory if non-empty).
//  2. ./cluster-config.yaml in the current working directory — the
//     historical default `l8k discover` writes when no path is given.
//  3. <deployment-files>/../cluster-config.yaml — the convention
//     `l8k discover --save-cluster-config <dir>/cluster-config.yaml
//     --save-deployment-files <dir>/deployment` produces, so an
//     operator running `l8k validate --deployment-files <dir>/deployment`
//     (or `l8k deploy …`) from anywhere finds the matching config.
//     Only checked when --deployment-files was bound on the active
//     subcommand (deploymentFiles != "").
//  4. <deployment-files>/cluster-config.yaml — fallback for users
//     who keep the config inside the deployment dir.
//  5. ./l8k-config.yaml — the generate-path's legacy default, used
//     when neither `cluster-config.yaml` nor a deployment-files-anchored
//     candidate is present.
//  6. <share-dir>/l8k-config.yaml — the installed system-wide config
//     (e.g. /usr/local/share/l8k/l8k-config.yaml), so that a `l8k`
//     invocation with no local config can still pick up the operator
//     defaults shipped with the binary.
//
// Callers decide how to react to a missing config: validate softens its
// per-check verdicts to "skipped"; deploy and generate treat it as an
// error (deploy because Phase 0 + preflight would otherwise skip with no
// useful signal; generate because there's nothing to render against).
func userConfigPath() string {
	candidates := []string{}
	if userConfig != "" {
		candidates = append(candidates, userConfig)
	}
	candidates = append(candidates, defaultUserConfigPath)
	if deploymentFiles != "" {
		candidates = append(candidates,
			filepath.Join(deploymentFiles, "..", "cluster-config.yaml"),
			filepath.Join(deploymentFiles, "cluster-config.yaml"),
		)
	}
	// Generate-path legacy fallbacks: ./l8k-config.yaml then the
	// installed system-wide config under <share-dir>.
	candidates = append(candidates,
		"l8k-config.yaml",
		filepath.Join(app.ResolveShareDir(), "l8k-config.yaml"),
	)
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// loadUserConfig reads the resolved user-config file and applies the
// embedded Network Operator release catalog. When opts is non-zero,
// values from opts override config-file fields (the same precedence
// applied by the generate path's launcher).
//
// Returns (cfg, path, nil) on success. Returns (nil, "", nil) when no
// user-config file is found — callers treat that as "no catalog data
// available" and proceed accordingly. A non-nil error means the file
// existed but couldn't be parsed (or the release lookup failed); the
// caller should bail since downstream code would be making decisions
// from incomplete state.
func loadUserConfig(opts options.Options) (*config.LaunchKitConfig, string, error) {
	path := userConfigPath()
	if path == "" {
		return nil, "", nil
	}
	cfg, err := config.LoadFullConfig(path, log.Log)
	if err != nil {
		return nil, path, fmt.Errorf("load %s: %w", path, err)
	}
	if cfg == nil {
		return nil, path, fmt.Errorf("user-config %s is empty", path)
	}
	if err := networkoperatorplugin.ApplyNetworkOperatorRelease(opts, cfg); err != nil {
		return nil, path, fmt.Errorf("apply release catalog: %w", err)
	}
	// Apply CLI namespace override. Mirror of the same branch in
	// NetworkOperatorPlugin.ApplyOptionsToConfig so the standalone
	// `l8k deploy` / `l8k validate` paths honour --network-operator-namespace
	// the same way the root-pipeline generate path does (which goes through
	// the launcher's plugin.ApplyOptionsToConfig instead of this helper).
	if opts.NetworkOperatorNamespace != "" {
		if cfg.NetworkOperator == nil {
			cfg.NetworkOperator = &config.NetworkOperatorConfig{}
		}
		cfg.NetworkOperator.Namespace = opts.NetworkOperatorNamespace
	}
	return cfg, path, nil
}
