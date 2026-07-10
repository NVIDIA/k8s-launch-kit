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
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
)

var (
	presetDir    string
	presetRepo   string
	presetBranch string
)

var presetCmd = &cobra.Command{
	Use:   "preset",
	Short: "Manage predefined cluster configuration presets",
	Long: `Manage predefined topology presets for known machine types.

Presets provide authoritative traffic classification, rail assignments, and
NUMA/GPU topology metadata for known hardware configurations. During cluster
discovery, presets are automatically matched and applied when the machine type
matches and PCI topology validates.`,
}

var presetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available presets",
	Long:  "List topology presets from --config-dir, the legacy local directory, or the embedded catalog.",
	Example: `  l8k preset list
	  l8k preset list --config-dir /etc/l8k
	  l8k preset list --output json`,
	Run: func(cmd *cobra.Command, args []string) {
		catalog, _, err := presetCatalogForConfigDir(configDir)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
			return
		}
		summaries, skipped, err := catalog.ListPresetSummaries()
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
			return
		}
		source := catalog.Source()

		if len(summaries) == 0 && len(skipped) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "No presets found in %s\n", source)
			return
		}

		if len(summaries) > 0 {
			nameW, machineW := len("NAME"), len("MACHINETYPE")
			for _, s := range summaries {
				if len(s.DirName) > nameW {
					nameW = len(s.DirName)
				}
				if len(s.MachineType) > machineW {
					machineW = len(s.MachineType)
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Available presets (%s):\n", source)
			fmt.Fprintf(cmd.OutOrStdout(), "  %-*s  %-*s  %s\n", nameW, "NAME", machineW, "MACHINETYPE", "GPUTYPE")
			for _, s := range summaries {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-*s  %-*s  %s\n", nameW, s.DirName, machineW, s.MachineType, s.GPUType)
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "No valid presets found in %s\n", source)
		}

		if len(skipped) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n%d preset(s) skipped:\n", len(skipped))
			for _, s := range skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "  %s — %s\n", s.DirName, s.Reason)
			}
		}
	},
}

var presetUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Download latest presets from GitHub",
	Long: `Download the latest predefined topology presets from the GitHub repository.

Uses the GitHub API to list and fetch preset files. Set GITHUB_TOKEN
environment variable for authenticated requests (avoids rate limits).`,
	Example: `  l8k preset update
	  l8k preset update --repo nvidia/k8s-launch-kit --branch main
	  l8k preset update --config-dir /etc/l8k
	  l8k preset update --dir /usr/local/share/l8k/presets`,
	RunE: func(cmd *cobra.Command, args []string) error {
		destination := presetDir
		if destination == "" && configDir != "" {
			resolved, err := assets.ResolveConfigDir(configDir)
			if err != nil {
				return fmt.Errorf("invalid --config-dir: %w", err)
			}
			destination = resolved.PresetsDir
			if destination == "" {
				destination = filepath.Join(resolved.Root, assets.PresetsDirName)
			}
		}
		opts := presets.DownloadOptions{
			Repo:   presetRepo,
			Branch: presetBranch,
			Dir:    destination,
		}

		downloaded, err := presets.DownloadPresets(context.Background(), opts)
		if err != nil {
			return fmt.Errorf("preset update failed: %w", err)
		}

		if len(downloaded) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No presets found in remote repository.")
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Downloaded %d preset(s):\n", len(downloaded))
		for _, name := range downloaded {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", name)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(presetCmd)
	presetCmd.AddCommand(presetListCmd)
	presetCmd.AddCommand(presetUpdateCmd)

	presetUpdateCmd.Flags().StringVar(&presetDir, "dir", "", "Destination directory for downloaded presets (default: auto-resolve)")
	presetUpdateCmd.Flags().StringVar(&presetRepo, "repo", "nvidia/k8s-launch-kit", "GitHub repository to download presets from")
	presetUpdateCmd.Flags().StringVar(&presetBranch, "branch", "main", "Git branch to download presets from")

	setFlagGroup(presetUpdateCmd, "dir", GroupCommon)
	setFlagGroup(presetUpdateCmd, "repo", GroupCommon)
	setFlagGroup(presetUpdateCmd, "branch", GroupCommon)
}
