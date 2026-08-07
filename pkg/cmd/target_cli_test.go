// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestSetFlagTargetScope(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var value string
	cmd.Flags().StringVar(&value, "value", "", "test value")

	setFlagTargetScope(cmd, []target.Name{target.Host}, "value", "missing")

	flag := cmd.Flags().Lookup("value")
	require.NotNil(t, flag)
	assert.Equal(t, []string{"host"}, flag.Annotations[flagTargetsAnnotation])
	actual, err := flagTargetScope(flag)
	require.NoError(t, err)
	assert.Equal(t, []target.Name{target.Host}, actual)
}

func TestValidateExplicitFlagTargets(t *testing.T) {
	t.Run("target-agnostic flag", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		var output string
		cmd.Flags().StringVar(&output, "output", "text", "output")
		setFlagTargetScope(cmd, []target.Name{target.Host, target.DPF}, "output")
		require.NoError(t, cmd.ParseFlags([]string{"--output", "json"}))
		require.NoError(t, validateExplicitFlagTargets(cmd, target.DPF))
	})

	t.Run("host default is ignored for dpf", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		var connectivity bool
		cmd.Flags().BoolVar(&connectivity, "connectivity", true, "connectivity")
		setFlagTargetScope(cmd, []target.Name{target.Host}, "connectivity")
		require.NoError(t, validateExplicitFlagTargets(cmd, target.DPF))
	})

	t.Run("explicit false remains explicit", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		var connectivity bool
		cmd.Flags().BoolVar(&connectivity, "connectivity", true, "connectivity")
		setFlagTargetScope(cmd, []target.Name{target.Host}, "connectivity")
		require.NoError(t, cmd.ParseFlags([]string{"--connectivity=false"}))

		err := validateExplicitFlagTargets(cmd, target.DPF)
		var scopeErr *flagTargetError
		require.ErrorAs(t, err, &scopeErr)
		assert.Equal(t, []string{"connectivity"}, scopeErr.Flags)
		assert.Contains(t, err.Error(), `--connectivity (targets: host)`)
	})

	t.Run("multiple flags are sorted", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		var alpha, zeta string
		cmd.Flags().StringVar(&zeta, "zeta", "", "zeta")
		cmd.Flags().StringVar(&alpha, "alpha", "", "alpha")
		setFlagTargetScope(cmd, []target.Name{target.Host}, "alpha", "zeta")
		require.NoError(t, cmd.ParseFlags([]string{"--zeta", "z", "--alpha", "a"}))

		err := validateExplicitFlagTargets(cmd, target.DPF)
		var scopeErr *flagTargetError
		require.ErrorAs(t, err, &scopeErr)
		assert.Equal(t, []string{"alpha", "zeta"}, scopeErr.Flags)
	})

	t.Run("changed flag must declare ownership", func(t *testing.T) {
		cmd := &cobra.Command{Use: "test"}
		var value string
		cmd.Flags().StringVar(&value, "value", "", "value")
		require.NoError(t, cmd.ParseFlags([]string{"--value", "set"}))
		require.ErrorContains(t, validateExplicitFlagTargets(cmd, target.Host), "has no target ownership metadata")
	})

	t.Run("nil command", func(t *testing.T) {
		require.ErrorContains(t, validateExplicitFlagTargets(nil, target.Host), "must not be nil")
	})
}

func TestTargetAwareCommandsDeclareFlagOwnershipAndHelpGroup(t *testing.T) {
	commands := []*cobra.Command{rootCmd, discoverCmd, generateCmd, deployCmd, validateCmd}
	for _, command := range commands {
		t.Run(command.Name(), func(t *testing.T) {
			seen := map[string]bool{}
			visit := func(flag *pflag.Flag) {
				if flag.Name == "help" || seen[flag.Name] {
					return
				}
				seen[flag.Name] = true
				_, err := flagTargetScope(flag)
				assert.NoErrorf(t, err, "%s flag --%s", command.CommandPath(), flag.Name)
				assert.NotEmptyf(t, flag.Annotations[flagGroupAnnotation],
					"%s flag --%s has no help group", command.CommandPath(), flag.Name)
			}
			command.LocalFlags().VisitAll(visit)
			command.PersistentFlags().VisitAll(visit)
			command.InheritedFlags().VisitAll(visit)
			assert.NotEmpty(t, seen)
		})
	}
}

func TestImportantFlagScopes(t *testing.T) {
	tests := []struct {
		command  *cobra.Command
		flag     string
		expected []target.Name
	}{
		{command: rootCmd, flag: "target", expected: []target.Name{target.Host, target.DPF}},
		{command: rootCmd, flag: "output", expected: []target.Name{target.Host, target.DPF}},
		{command: rootCmd, flag: "fabric", expected: []target.Name{target.Host}},
		{command: discoverCmd, flag: "kubeconfig", expected: []target.Name{target.Host}},
		{command: generateCmd, flag: "deploy", expected: []target.Name{target.Host, target.DPF}},
		{command: deployCmd, flag: "dry-run", expected: []target.Name{target.Host, target.DPF}},
		{command: validateCmd, flag: "connectivity", expected: []target.Name{target.Host}},
	}
	for _, tt := range tests {
		t.Run(tt.command.Name()+"/"+tt.flag, func(t *testing.T) {
			flag := tt.command.Flags().Lookup(tt.flag)
			if flag == nil {
				flag = tt.command.PersistentFlags().Lookup(tt.flag)
			}
			require.NotNil(t, flag)
			actual, err := flagTargetScope(flag)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestBindTargetCommand(t *testing.T) {
	originalTarget := targetName
	originalOutput := outputFormat
	originalDryRun := dryRunFlag
	t.Cleanup(func() {
		targetName = originalTarget
		outputFormat = originalOutput
		dryRunFlag = originalDryRun
	})

	newCommand := func() *cobra.Command {
		command := &cobra.Command{Use: "test"}
		addTargetFlag(command)
		return command
	}

	t.Run("omitted target runs host", func(t *testing.T) {
		targetName = string(target.Host)
		command := newCommand()
		ran := false
		operation, err := bindTargetCommand(command, target.Generate, func() { ran = true })
		require.NoError(t, err)
		require.NoError(t, operation.Run(context.Background()))
		assert.True(t, ran)
	})

	t.Run("explicit host runs same operation", func(t *testing.T) {
		command := newCommand()
		require.NoError(t, command.ParseFlags([]string{"--target", "host"}))
		ran := false
		operation, err := bindTargetCommand(command, target.Generate, func() { ran = true })
		require.NoError(t, err)
		require.NoError(t, operation.Run(context.Background()))
		assert.True(t, ran)
	})

	t.Run("dpf returns phase capability error", func(t *testing.T) {
		command := newCommand()
		require.NoError(t, command.ParseFlags([]string{"--target", "dpf"}))
		operation, err := bindTargetCommand(command, target.Validate, func() { t.Fatal("host must not run") })
		var unavailable *target.PhaseUnavailableError
		require.ErrorAs(t, err, &unavailable)
		assert.Equal(t, target.DPF, unavailable.Name)
		assert.Equal(t, target.Validate, unavailable.Phase)
		assert.Nil(t, operation)
	})

	t.Run("unknown target reports registered targets", func(t *testing.T) {
		command := newCommand()
		require.NoError(t, command.ParseFlags([]string{"--target", "future"}))
		operation, err := bindTargetCommand(command, target.Discover, func() { t.Fatal("host must not run") })
		var unknown *target.UnknownTargetError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, []target.Name{target.DPF, target.Host}, unknown.Supported)
		assert.Nil(t, operation)
	})
}

func TestTargetDescriptors(t *testing.T) {
	descriptors := targetDescriptors()
	require.Len(t, descriptors, 2)
	assert.Equal(t, target.Host, descriptors[0].Name)
	assert.Equal(t, target.DPF, descriptors[1].Name)
	for _, phase := range target.PublicPhases() {
		assert.True(t, descriptors[0].Capability(phase).Available)
		assert.False(t, descriptors[1].Capability(phase).Available)
		assert.NotEmpty(t, descriptors[1].Capability(phase).Reason)
	}
}

func TestTargetCLIProcess(t *testing.T) {
	t.Run("schema exposes target and flag capabilities", func(t *testing.T) {
		output, exitCode := runCLIHelper(t, "schema")
		require.Equal(t, 0, exitCode)
		var actual struct {
			DefaultTarget string `json:"defaultTarget"`
			Targets       []struct {
				Name   string                       `json:"name"`
				Phases map[string]target.Capability `json:"phases"`
			} `json:"targets"`
			Flags map[string]flagSchema `json:"flags"`
		}
		require.NoError(t, json.Unmarshal([]byte(output), &actual))
		assert.Equal(t, "host", actual.DefaultTarget)
		require.Len(t, actual.Targets, 2)
		assert.Equal(t, "host", actual.Targets[0].Name)
		assert.Equal(t, "dpf", actual.Targets[1].Name)
		assert.True(t, actual.Targets[0].Phases["validate"].Available)
		assert.False(t, actual.Targets[1].Phases["validate"].Available)
		assert.Equal(t, []string{"host", "dpf"}, actual.Flags["--output"].Targets)
		assert.Equal(t, []string{"host"}, actual.Flags["--fabric"].Targets)
	})

	t.Run("omitted and explicit host preserve output and exit code", func(t *testing.T) {
		omittedOutput, omittedExit := runCLIHelper(t,
			"generate", "--user-config", "/definitely/missing/config.yaml", "--output", "json")
		explicitOutput, explicitExit := runCLIHelper(t,
			"generate", "--target", "host", "--user-config", "/definitely/missing/config.yaml", "--output", "json")

		assert.Equal(t, omittedExit, explicitExit)
		assert.Equal(t, omittedOutput, explicitOutput)
		assert.Equal(t, 1, omittedExit)
		assert.Contains(t, omittedOutput, "cluster config file does not exist")
	})

	for _, phase := range target.PublicPhases() {
		t.Run("dpf "+string(phase)+" capability error", func(t *testing.T) {
			output, exitCode := runCLIHelper(t, string(phase), "--target", "dpf")
			assert.Equal(t, 2, exitCode)
			assert.Contains(t, output, `invalid target invocation`)
			assert.Contains(t, output, `target "dpf" does not implement phase "`+string(phase)+`"`)
		})
	}

	t.Run("root dpf pipeline cannot enter host path", func(t *testing.T) {
		output, exitCode := runCLIHelper(t, "--target", "dpf")
		assert.Equal(t, 2, exitCode)
		assert.Contains(t, output, `target "dpf" does not implement phase "pipeline"`)
	})

	t.Run("explicit false host flag is rejected for dpf", func(t *testing.T) {
		output, exitCode := runCLIHelper(t, "validate", "--target", "dpf", "--connectivity=false")
		assert.Equal(t, 2, exitCode)
		assert.Contains(t, output, `--connectivity (targets: host)`)
	})

	t.Run("dpf capability errors preserve json output mode", func(t *testing.T) {
		output, exitCode := runCLIHelper(t, "discover", "--target", "dpf", "--output", "json")
		assert.Equal(t, 2, exitCode)
		assert.Contains(t, output, `"code": "VALIDATION_ERROR"`)
		assert.Contains(t, output, `"success": false`)
	})

	t.Run("unknown target is distinct from unavailable phase", func(t *testing.T) {
		output, exitCode := runCLIHelper(t, "discover", "--target", "future")
		assert.Equal(t, 2, exitCode)
		assert.Contains(t, output, `unknown target "future"`)
		assert.Contains(t, output, `registered targets: [dpf host]`)
	})
}

func TestTargetCLIHelperProcess(t *testing.T) {
	if os.Getenv("L8K_TARGET_CLI_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(99)
	}
	rootCmd.SetArgs(os.Args[separator+1:])
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func runCLIHelper(t *testing.T, args ...string) (string, int) {
	t.Helper()
	commandArgs := []string{"-test.run=^TestTargetCLIHelperProcess$", "--"}
	commandArgs = append(commandArgs, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(os.Environ(), "L8K_TARGET_CLI_HELPER=1")
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run helper %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output), exitErr.ExitCode()
}
