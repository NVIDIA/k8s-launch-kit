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

package host

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	yaml "sigs.k8s.io/yaml"
)

var connectedGPUNamePattern = regexp.MustCompile(`^GPU[0-9]+$`)

type connectivityConfigContract struct {
	Profile *struct {
		Routing string `yaml:"routing"`
	} `yaml:"profile"`
	Validation *struct {
		GPUDirect *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"gpuDirect"`
	} `yaml:"validation"`
}

func validationOverrides(request ValidateRequest) []configinput.Override {
	var overrides []configinput.Override
	add := func(set bool, flag, path string, value any) {
		if set {
			overrides = append(overrides, configinput.Override{Flag: flag, Path: path, Value: value})
		}
	}
	add(request.Connectivity.Set, "connectivity", "validation.connectivity", request.Connectivity.Value)
	add(request.Mode.Set, "validation-mode", "validation.mode", request.Mode.Value)
	add(request.Checks.Set, "validation-checks", "validation.checks", request.Checks.Value)
	add(request.RDMAPIterations.Set, "rdma-rping-iterations", "validation.rdma.rpingIterations", request.RDMAPIterations.Value)
	add(request.RDMAIBWriteSize.Set, "rdma-ib-write-size", "validation.rdma.ibWriteSize", request.RDMAIBWriteSize.Value)
	add(request.RDMAMinBandwidth.Set, "rdma-ib-write-min-bandwidth-gbps", "validation.rdma.ibWriteMinBandwidthGbps", request.RDMAMinBandwidth.Value)
	return overrides
}

// validationOptions puts all CLI fields into the resolver before it validates
// user YAML, including values that repair an invalid lower-precedence field.
func validationOptions(request ValidateRequest) options.Options {
	opts := options.Options{
		Flavor:                     request.Flavor,
		ConfigDir:                  request.ConfigDir,
		UserConfig:                 request.UserConfig,
		NetworkOperatorNamespace:   request.OperatorNamespace,
		SkipNetworkOperatorHelm:    request.SkipNetworkOperatorHelm.Value,
		SkipNetworkOperatorHelmSet: request.SkipNetworkOperatorHelm.Set,
	}
	opts.ConfigInputs = configflags.Infer(opts)
	opts.ConfigInputs.Overrides = append(opts.ConfigInputs.Overrides, validationOverrides(request)...)
	return opts
}

func applyValidationOverrides(request ValidateRequest, validationCfg *config.ValidationConfig) error {
	if validationCfg == nil {
		return fmt.Errorf("validation config must not be nil")
	}
	overrides := validationOverrides(request)
	if err := config.ApplyOverrides(&config.LaunchKitConfig{Validation: validationCfg}, overrides); err != nil {
		return err
	}
	present := config.PathSet{}
	for _, override := range overrides {
		present.Add(override.Path)
	}
	config.NormalizeValidationConfigWithPresence(validationCfg, present)
	return config.ValidateValidationConfig(validationCfg)
}

// validateRequiredConnectivityConfig verifies the user-owned decisions that
// cannot be inferred safely from a test DaemonSet or from l8k defaults.
func validateRequiredConnectivityConfig(
	path string,
	cfg *config.LaunchKitConfig,
	checks []connectivity.Check,
) error {
	if path == "" {
		return fmt.Errorf("a user-owned cluster-config.yaml is required when connectivity validation is enabled")
	}
	if cfg == nil {
		return fmt.Errorf("cluster config %s is empty", path)
	}
	if config.IsEffectiveConfigPath(path) {
		return validateResolvedConnectivityConfig(path, cfg, checks)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read required cluster config %s: %w", path, err)
	}
	var contract connectivityConfigContract
	if err := yaml.Unmarshal(data, &contract); err != nil {
		return fmt.Errorf("parse required cluster config %s: %w", path, err)
	}

	if contract.Profile == nil || strings.TrimSpace(contract.Profile.Routing) == "" {
		return fmt.Errorf("cluster config %s must explicitly set profile.routing", path)
	}
	routing := strings.TrimSpace(contract.Profile.Routing)
	if routing != config.RoutingSourceBased && routing != config.RoutingDestinationBased {
		return fmt.Errorf("cluster config %s profile.routing must be %q or %q, got %q",
			path, config.RoutingSourceBased, config.RoutingDestinationBased, routing)
	}
	if contract.Validation == nil || contract.Validation.GPUDirect == nil ||
		contract.Validation.GPUDirect.Enabled == nil {
		return fmt.Errorf("cluster config %s must explicitly set validation.gpuDirect.enabled", path)
	}

	if cfg.Validation != nil && cfg.Validation.GPUDirect.Enabled &&
		connectivityChecksContain(checks, connectivity.CheckGPUDirectDMABuf) {
		if err := validateGPUDirectTopology(cfg.ClusterConfig); err != nil {
			return fmt.Errorf("cluster config %s cannot support GPUDirect connectivity validation: %w", path, err)
		}
	}
	return nil
}

func validateResolvedConnectivityConfig(path string, cfg *config.LaunchKitConfig, checks []connectivity.Check) error {
	if cfg.Profile == nil || strings.TrimSpace(cfg.Profile.Routing) == "" {
		return fmt.Errorf("resolved config %s has no profile.routing", path)
	}
	routing := strings.TrimSpace(cfg.Profile.Routing)
	if routing != config.RoutingSourceBased && routing != config.RoutingDestinationBased {
		return fmt.Errorf("resolved config %s profile.routing must be %q or %q, got %q",
			path, config.RoutingSourceBased, config.RoutingDestinationBased, routing)
	}
	if cfg.Validation == nil {
		return fmt.Errorf("resolved config %s has no validation settings", path)
	}
	if cfg.Validation.GPUDirect.Enabled && connectivityChecksContain(checks, connectivity.CheckGPUDirectDMABuf) {
		if err := validateGPUDirectTopology(cfg.ClusterConfig); err != nil {
			return fmt.Errorf("resolved config %s cannot support GPUDirect connectivity validation: %w", path, err)
		}
	}
	return nil
}

func connectivityChecksContain(checks []connectivity.Check, want connectivity.Check) bool {
	for _, check := range checks {
		if check == want {
			return true
		}
	}
	return false
}

func validateGPUDirectTopology(groups []config.ClusterConfig) error {
	if len(groups) == 0 {
		return fmt.Errorf("clusterConfig must contain at least one worker group")
	}

	workerGroups := map[string]string{}
	for groupIndex, group := range groups {
		groupName := strings.TrimSpace(group.Identifier)
		if groupName == "" {
			groupName = fmt.Sprintf("clusterConfig[%d]", groupIndex)
		}
		if len(group.WorkerNodes) == 0 {
			return fmt.Errorf("group %s must list workerNodes", groupName)
		}
		for _, worker := range group.WorkerNodes {
			worker = strings.TrimSpace(worker)
			if worker == "" {
				return fmt.Errorf("group %s contains an empty workerNodes entry", groupName)
			}
			if previousGroup, exists := workerGroups[worker]; exists {
				return fmt.Errorf("worker %q belongs to both %s and %s", worker, previousGroup, groupName)
			}
			workerGroups[worker] = groupName
		}

		eastWestPFs := 0
		for pfIndex, pf := range group.PFs {
			if pf.Traffic != "east-west" {
				continue
			}
			eastWestPFs++
			if pf.Rail == nil || *pf.Rail < 0 {
				return fmt.Errorf("group %s east-west PF %d must set a non-negative rail", groupName, pfIndex)
			}
			if !connectedGPUNamePattern.MatchString(strings.TrimSpace(pf.ConnectedGPU)) {
				return fmt.Errorf("group %s east-west PF %d must set connectedGPU as GPU<N>", groupName, pfIndex)
			}
		}
		if eastWestPFs == 0 {
			return fmt.Errorf("group %s must contain at least one east-west PF", groupName)
		}
	}
	return nil
}
