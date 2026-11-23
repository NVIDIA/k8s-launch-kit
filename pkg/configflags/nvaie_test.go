// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package configflags

import (
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNVAIEFlagAllAdvertisedScopes(t *testing.T) {
	for _, scope := range []Scope{ScopeRoot, ScopeDiscover, ScopeGenerate, ScopeValidate} {
		t.Run(string(scope), func(t *testing.T) {
			var opts options.Options
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			require.NoError(t, Bind(flags, &opts, scope))
			require.NoError(t, flags.Parse([]string{"--nvaie-release="}))
			require.NoError(t, Collect(flags, &opts))
			require.Len(t, opts.ConfigInputs.Overrides, 1)
			require.Equal(t, "nvaieRelease", opts.ConfigInputs.Overrides[0].Path)
			require.Equal(t, "", opts.ConfigInputs.Overrides[0].Value)
		})
	}
}
