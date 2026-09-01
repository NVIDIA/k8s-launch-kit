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
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	hosttarget "github.com/nvidia/k8s-launch-kit/pkg/target/host"
)

const flagTargetsAnnotation = "launchkit.nvidia.com/targets"

var targetName = string(target.Host)

// flagTargetError reports target-owned flags that were explicitly set for a
// different target. Defaults never cause this error because validation walks
// only pflag's Changed set.
type flagTargetError struct {
	Selected target.Name
	Flags    []string
	Allowed  map[string][]target.Name
}

func (e *flagTargetError) Error() string {
	parts := make([]string, 0, len(e.Flags))
	for _, name := range e.Flags {
		parts = append(parts, fmt.Sprintf("--%s (targets: %s)", name, joinTargetNames(e.Allowed[name])))
	}
	return fmt.Sprintf("flags not valid for target %q: %s", e.Selected, strings.Join(parts, ", "))
}

func joinTargetNames(names []target.Name) string {
	values := make([]string, len(names))
	for index, name := range names {
		values[index] = string(name)
	}
	return strings.Join(values, ", ")
}

func addTargetFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(
		&targetName,
		"target",
		string(target.Host),
		"Deployment target: host (default) or dpf (reserved; phases are unavailable until the DPF driver is added)",
	)
	setFlagGroup(cmd, "target", GroupTarget)
	setFlagTargetScope(cmd, []target.Name{target.Host, target.DPF}, "target")
}

// setFlagTargetScope marks flag names with the targets that own their exact
// semantics. It mirrors setFlagGroup's local-then-persistent lookup because
// annotations are applied during command construction, before Cobra merges
// inherited persistent flags.
func setFlagTargetScope(cmd *cobra.Command, targets []target.Name, names ...string) {
	values := make([]string, len(targets))
	for index, name := range targets {
		values[index] = string(name)
	}
	for _, name := range names {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			flag = cmd.PersistentFlags().Lookup(name)
		}
		if flag == nil {
			continue
		}
		if flag.Annotations == nil {
			flag.Annotations = map[string][]string{}
		}
		flag.Annotations[flagTargetsAnnotation] = append([]string(nil), values...)
	}
}

func flagTargetScope(flag *pflag.Flag) ([]target.Name, error) {
	values := flag.Annotations[flagTargetsAnnotation]
	if len(values) == 0 {
		return nil, fmt.Errorf("flag --%s has no target ownership metadata", flag.Name)
	}
	names := make([]target.Name, 0, len(values))
	for _, value := range values {
		name, err := target.ParseName(value)
		if err != nil {
			return nil, fmt.Errorf("flag --%s has invalid target metadata: %w", flag.Name, err)
		}
		names = append(names, name)
	}
	return names, nil
}

func validateExplicitFlagTargets(cmd *cobra.Command, selected target.Name) error {
	if cmd == nil {
		return fmt.Errorf("command must not be nil")
	}
	invalid := &flagTargetError{Selected: selected, Allowed: map[string][]target.Name{}}
	var metadataErr error
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if metadataErr != nil {
			return
		}
		// The selector itself is validated by registry lookup below. Its
		// annotation describes currently advertised targets for help/schema,
		// not a restriction that should mask UnknownTargetError.
		if flag.Name == "target" {
			return
		}
		allowed, err := flagTargetScope(flag)
		if err != nil {
			metadataErr = err
			return
		}
		for _, name := range allowed {
			if name == selected {
				return
			}
		}
		invalid.Flags = append(invalid.Flags, flag.Name)
		invalid.Allowed[flag.Name] = allowed
	})
	if metadataErr != nil {
		return metadataErr
	}
	if len(invalid.Flags) == 0 {
		return nil
	}
	sort.Strings(invalid.Flags)
	return invalid
}

type unavailableTargetDriver struct {
	descriptor target.Descriptor
}

func (d unavailableTargetDriver) Descriptor() target.Descriptor { return d.descriptor }

func (d unavailableTargetDriver) Bind(invocation target.Invocation) (target.Operation, error) {
	return nil, &target.PhaseUnavailableError{
		Name:   invocation.Target,
		Phase:  invocation.Phase,
		Reason: d.descriptor.Capability(invocation.Phase).Reason,
	}
}

func dpfTargetDescriptor() target.Descriptor {
	const reason = "the DPF target name and CLI boundary are reserved, but the DPF driver is not available in this build"
	unavailable := target.Capability{Available: false, Reason: reason}
	return target.Descriptor{
		Name:        target.DPF,
		Description: "DPU-plane provisioning through NVIDIA DPF Operator",
		Phases: map[target.Phase]target.Capability{
			target.Discover: unavailable,
			target.Generate: unavailable,
			target.Deploy:   unavailable,
			target.Validate: unavailable,
			target.Pipeline: unavailable,
		},
	}
}

func targetDescriptors() []target.Descriptor {
	return []target.Descriptor{hosttarget.Descriptor(), dpfTargetDescriptor()}
}

func bindTargetCommand(cmd *cobra.Command, phase target.Phase, hostDriver target.Driver) (target.Operation, error) {
	selected, err := target.ParseName(targetName)
	if err != nil {
		return nil, err
	}
	if err := validateExplicitFlagTargets(cmd, selected); err != nil {
		return nil, err
	}

	registry, err := target.NewRegistry(
		hostDriver,
		unavailableTargetDriver{descriptor: dpfTargetDescriptor()},
	)
	if err != nil {
		return nil, fmt.Errorf("construct target registry: %w", err)
	}

	var timeout time.Duration
	switch phase {
	case target.Pipeline:
		timeout = deployTimeoutRoot
	case target.Deploy:
		timeout = deployTimeout
	}
	return registry.Bind(target.Invocation{
		Target: selected,
		Phase:  phase,
		Output: target.OutputOptions{
			Format:      outputFormat,
			Quiet:       quietFlag,
			AutoApprove: yesFlag || outputFormat == "json",
		},
		Execution: target.ExecutionOptions{
			DryRun:  dryRunFlag,
			Timeout: timeout,
		},
	})
}

func runTargetCommand(cmd *cobra.Command, phase target.Phase, hostDriver target.Driver) {
	operation, err := bindTargetCommand(cmd, phase, hostDriver)
	if err != nil {
		var structured *apperrors.StructuredError
		if errors.As(err, &structured) {
			exitWithError(structured, outputFormat)
		}
		exitWithError(apperrors.NewValidationError(
			"invalid target invocation",
			err,
			targetErrorSuggestion(err),
		), outputFormat)
	}
	if err := operation.Run(cmd.Context()); err != nil {
		if code, ok := apperrors.IsExitStatus(err); ok {
			os.Exit(code)
		}
		if apperrors.IsReported(err) {
			os.Exit(apperrors.ExitCodeFromError(err))
		}
		var structured *apperrors.StructuredError
		if !errors.As(err, &structured) {
			structured = apperrors.NewGeneralError(err.Error(), err)
		}
		exitWithError(structured, outputFormat)
	}
}

func targetErrorSuggestion(err error) string {
	var flags *flagTargetError
	if errors.As(err, &flags) {
		return "Remove flags owned by another target, or select --target host for the existing Network Operator workflow"
	}
	var unavailable *target.PhaseUnavailableError
	if errors.As(err, &unavailable) {
		return "Use --target host for the current workflow; inspect 'l8k schema' for target phase availability"
	}
	var unknown *target.UnknownTargetError
	if errors.As(err, &unknown) {
		return "Use a target listed by 'l8k schema'"
	}
	return "Run 'l8k schema' to inspect target and flag capabilities"
}

func markRootTargetScopes() {
	setFlagTargetScope(rootCmd, []target.Name{target.Host, target.DPF},
		"target", "output", "log-level", "log-file", "yes", "quiet", "dry-run", "deploy", "deploy-timeout")
	setFlagTargetScope(rootCmd, []target.Name{target.Host},
		"enabled-plugins", "discover-cluster-config", "save-cluster-config", "user-config",
		"network-operator-namespace", "network-operator-release", "skip-network-operator-helm", "fabric", "deployment-type",
		"multirail", "routing", "ignore-arp", "spectrum-x", "multiplane-mode", "number-of-planes",
		"topology-scheme", "ip-version", "topology-file", "spectrum-x-config",
		"spectrum-x-configmap-name", "groups", "gpu-type", "node-selector", "collapse-nic-rails",
		"for", "image-pull-secrets", "save-deployment-files", "network-namespaces",
		"enable-doca-driver", "workload-manifest", "kubeconfig", "config-dir")
}

func markDiscoverTargetScopes() {
	setFlagTargetScope(discoverCmd, []target.Name{target.Host},
		"kubeconfig", "user-config", "save-cluster-config", "network-operator-namespace",
		"network-operator-release", "node-selector", "image-pull-secrets", "enabled-plugins",
		"keep-namespace", "collapse-nic-rails", "fabric", "deployment-type", "multirail",
		"routing", "ignore-arp", "spectrum-x", "multiplane-mode", "number-of-planes",
		"topology-scheme", "ip-version", "topology-file", "spectrum-x-config",
		"spectrum-x-configmap-name")
}

func markGenerateTargetScopes() {
	setFlagTargetScope(generateCmd, []target.Name{target.Host, target.DPF}, "deploy", "dry-run")
	setFlagTargetScope(generateCmd, []target.Name{target.Host},
		"user-config", "fabric", "deployment-type", "multirail", "routing", "ignore-arp",
		"spectrum-x", "multiplane-mode", "number-of-planes", "topology-scheme", "ip-version",
		"topology-file", "spectrum-x-config", "spectrum-x-configmap-name", "groups", "gpu-type",
		"for", "node-selector", "save-deployment-files", "network-namespaces", "enable-doca-driver",
		"workload-manifest", "network-operator-namespace", "network-operator-release", "skip-network-operator-helm",
		"image-pull-secrets", "enabled-plugins", "kubeconfig", "overwrite-existing")
}

func markDeployTargetScopes() {
	setFlagTargetScope(deployCmd, []target.Name{target.Host, target.DPF}, "dry-run", "deploy-timeout")
	setFlagTargetScope(deployCmd, []target.Name{target.Host},
		"kubeconfig", "deployment-files", "network-operator-namespace", "user-config", "overwrite-existing", "skip-network-operator-helm")
}

func markValidateTargetScopes() {
	setFlagTargetScope(validateCmd, []target.Name{target.Host},
		"kubeconfig", "deployment-files", "user-config", "network-operator-namespace", "skip-network-operator-helm", "connectivity",
		"keep", "connectivity-timeout", "validation-mode", "validation-checks",
		"rdma-rping-iterations", "rdma-ib-write-size", "rdma-ib-write-min-bandwidth-gbps",
		"wait", "report-path")
}
