// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

func mustBindConfigFlags(cmd *cobra.Command, opts *options.Options, scope configflags.Scope) {
	if err := configflags.Bind(cmd.Flags(), opts, scope); err != nil {
		panic(err)
	}
}

func mustCollectConfigFlags(cmd *cobra.Command, template options.Options) options.Options {
	opts := template
	if err := configflags.Collect(cmd.Flags(), &opts); err != nil {
		panic(err)
	}
	return opts
}
