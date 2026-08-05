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
	"bytes"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const flagGroupAnnotation = "flagGroup"

// Group keys are prefixed with their render order so the constant value
// also encodes ordering, leaving groupOrder as the single source of truth
// for what gets rendered.
const (
	GroupCommon        = "1-common"
	GroupDiscovery     = "2-discovery"
	GroupProfile       = "3-profile"
	GroupSpectrumX     = "4-spectrumx"
	GroupGeneration    = "5-generation"
	GroupDeploy        = "6-deploy"
	GroupClean         = "7-clean"
	GroupValidation    = "8-validation"
	GroupOutputLogging = "9-output-logging"
)

var groupOrder = []string{
	GroupCommon,
	GroupDiscovery,
	GroupProfile,
	GroupSpectrumX,
	GroupGeneration,
	GroupDeploy,
	GroupClean,
	GroupValidation,
	GroupOutputLogging,
}

var groupTitles = map[string]string{
	GroupCommon:        "Common Flags",
	GroupDiscovery:     "Discovery Flags",
	GroupProfile:       "Profile Selection Flags",
	GroupSpectrumX:     "Spectrum-X Flags",
	GroupGeneration:    "Generation Output Flags",
	GroupDeploy:        "Deploy Flags",
	GroupClean:         "Clean Flags",
	GroupValidation:    "Validation Flags",
	GroupOutputLogging: "Output & Logging Flags",
}

// setFlagGroup tags the named flag on cmd with the given group key.
// Looks up local flags first and falls back to persistent flags — cobra
// only merges persistent flags into Flags() at Execute time, so during
// init() Flags().Lookup misses anything registered via PersistentFlags.
// No-op when the flag is not registered on cmd, so a subcommand's init()
// can call it for every flag it might own without guarding each call.
func setFlagGroup(cmd *cobra.Command, name, group string) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		flag = cmd.PersistentFlags().Lookup(name)
	}
	if flag == nil {
		return
	}
	if flag.Annotations == nil {
		flag.Annotations = map[string][]string{}
	}
	flag.Annotations[flagGroupAnnotation] = []string{group}
}

// groupedFlagUsages renders cmd's flags as labelled sections — one
// section per non-empty declared group plus a trailing "Other Flags"
// bucket for any flag missing an annotation. Walks LocalFlags and
// InheritedFlags so persistent flags from a parent command are sectioned
// alongside the subcommand's own flags.
func groupedFlagUsages(cmd *cobra.Command) string {
	buckets := map[string]*pflag.FlagSet{}
	add := func(group string, f *pflag.Flag) {
		fs, ok := buckets[group]
		if !ok {
			fs = pflag.NewFlagSet(group, pflag.ContinueOnError)
			buckets[group] = fs
		}
		fs.AddFlag(f)
	}

	visit := func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		group := ""
		if vals := f.Annotations[flagGroupAnnotation]; len(vals) > 0 {
			group = vals[0]
		}
		// Cobra auto-injects --help on every command at Execute time,
		// after init() has run, so it can't be tagged via setFlagGroup.
		// Bin it alongside the other meta flags.
		if group == "" && f.Name == "help" {
			group = GroupOutputLogging
		}
		add(group, f)
	}

	cmd.LocalFlags().VisitAll(visit)
	cmd.InheritedFlags().VisitAll(visit)

	var buf bytes.Buffer
	first := true
	section := func(title string, fs *pflag.FlagSet) {
		usage := strings.TrimRight(fs.FlagUsagesWrapped(0), " \n")
		if usage == "" {
			return
		}
		if !first {
			buf.WriteString("\n\n")
		}
		first = false
		fmt.Fprintf(&buf, "%s:\n%s", title, usage)
	}

	for _, group := range groupOrder {
		if fs, ok := buckets[group]; ok {
			section(groupTitles[group], fs)
		}
	}
	if fs, ok := buckets[""]; ok {
		section("Other Flags", fs)
	}

	return buf.String()
}

// installGroupedUsage swaps in a usage template that calls
// groupedFlagUsages instead of cobra's default Flags / Global Flags
// pair. Cobra's Command.UsageTemplate() walks up to the parent when
// unset, so installing this on root propagates it to every subcommand.
func installGroupedUsage(root *cobra.Command) {
	cobra.AddTemplateFunc("groupedFlagUsages", groupedFlagUsages)
	root.SetUsageTemplate(groupedUsageTemplate)
}

// groupedUsageTemplate mirrors cobra's defaultUsageTemplate verbatim
// except for the two FlagUsages sections, which are replaced by a
// single groupedFlagUsages call.
const groupedUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if or .HasAvailableLocalFlags .HasAvailableInheritedFlags}}

{{groupedFlagUsages . | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`
