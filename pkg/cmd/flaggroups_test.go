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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetFlagGroup_NoOpWhenFlagAbsent(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	require.NotPanics(t, func() {
		setFlagGroup(cmd, "missing", GroupCommon)
	})
}

func TestSetFlagGroup_TagsExistingFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	var v string
	cmd.Flags().StringVar(&v, "alpha", "", "alpha flag")

	setFlagGroup(cmd, "alpha", GroupCommon)

	flag := cmd.Flags().Lookup("alpha")
	require.NotNil(t, flag)
	require.Equal(t, []string{GroupCommon}, flag.Annotations[flagGroupAnnotation])
}

func TestGroupedFlagUsages_RendersGroupsInOrderWithBlankLines(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	var common, profile, deploy string
	cmd.Flags().StringVar(&common, "common-flag", "", "common help")
	cmd.Flags().StringVar(&profile, "profile-flag", "", "profile help")
	cmd.Flags().StringVar(&deploy, "deploy-flag", "", "deploy help")

	setFlagGroup(cmd, "common-flag", GroupCommon)
	setFlagGroup(cmd, "profile-flag", GroupProfile)
	setFlagGroup(cmd, "deploy-flag", GroupDeploy)

	out := groupedFlagUsages(cmd)

	commonIdx := strings.Index(out, groupTitles[GroupCommon]+":")
	profileIdx := strings.Index(out, groupTitles[GroupProfile]+":")
	deployIdx := strings.Index(out, groupTitles[GroupDeploy]+":")
	require.NotEqual(t, -1, commonIdx, "Common Flags section missing:\n%s", out)
	require.NotEqual(t, -1, profileIdx, "Profile Selection Flags section missing:\n%s", out)
	require.NotEqual(t, -1, deployIdx, "Deploy Flags section missing:\n%s", out)
	assert.Less(t, commonIdx, profileIdx, "Common must precede Profile Selection")
	assert.Less(t, profileIdx, deployIdx, "Profile Selection must precede Deploy")

	// Each section's flag lands under its own heading.
	commonSection := out[commonIdx:profileIdx]
	assert.Contains(t, commonSection, "--common-flag")
	assert.NotContains(t, commonSection, "--profile-flag")
	assert.NotContains(t, commonSection, "--deploy-flag")

	// Sections are visually separated by a blank line.
	assert.Contains(t, out, "\n\n"+groupTitles[GroupProfile]+":")
	assert.Contains(t, out, "\n\n"+groupTitles[GroupDeploy]+":")
}

func TestGroupedFlagUsages_UntaggedFlagFallsIntoOtherFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	var tagged, untagged string
	cmd.Flags().StringVar(&tagged, "tagged", "", "tagged help")
	cmd.Flags().StringVar(&untagged, "stray", "", "stray help")

	setFlagGroup(cmd, "tagged", GroupCommon)

	out := groupedFlagUsages(cmd)
	assert.Contains(t, out, groupTitles[GroupCommon]+":")
	assert.Contains(t, out, "Other Flags:")

	otherIdx := strings.Index(out, "Other Flags:")
	require.NotEqual(t, -1, otherIdx)
	assert.Contains(t, out[otherIdx:], "--stray")
}

func TestGroupedFlagUsages_OmitsHiddenFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	var visible, hidden string
	cmd.Flags().StringVar(&visible, "visible", "", "visible help")
	cmd.Flags().StringVar(&hidden, "secret", "", "secret help")
	require.NoError(t, cmd.Flags().MarkHidden("secret"))

	setFlagGroup(cmd, "visible", GroupCommon)
	setFlagGroup(cmd, "secret", GroupCommon)

	out := groupedFlagUsages(cmd)
	assert.Contains(t, out, "--visible")
	assert.NotContains(t, out, "--secret")
}

func TestGroupedFlagUsages_IncludesInheritedFlagsFromParent(t *testing.T) {
	parent := &cobra.Command{Use: "parent"}
	child := &cobra.Command{Use: "child", Run: func(_ *cobra.Command, _ []string) {}}
	parent.AddCommand(child)

	var inherited string
	parent.PersistentFlags().StringVar(&inherited, "global-output", "", "global help")
	setFlagGroup(parent, "global-output", GroupOutputLogging)

	var local string
	child.Flags().StringVar(&local, "local-flag", "", "local help")
	setFlagGroup(child, "local-flag", GroupCommon)

	out := groupedFlagUsages(child)
	assert.Contains(t, out, groupTitles[GroupCommon]+":")
	assert.Contains(t, out, "--local-flag")
	assert.Contains(t, out, groupTitles[GroupOutputLogging]+":")
	assert.Contains(t, out, "--global-output")
}
