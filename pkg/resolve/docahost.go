// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// ApplyDOCAHostDefault disables the container only with complete host package
// evidence. Explicit user/CLI choices always win. It runs after canonical
// defaults so the hardware-derived false cannot be replaced by the default true.
func ApplyDOCAHostDefault(cfg *config.LaunchKitConfig, present config.PathSet) bool {
	if cfg == nil || present.Has("docaDriver.enable") {
		return false
	}
	installed, total, membershipKnown := config.DOCAHostCoverage(cfg)
	if !membershipKnown || total == 0 || installed != total {
		return false
	}
	if cfg.DOCADriver == nil {
		cfg.DOCADriver = &config.DOCADriverConfig{}
	}
	cfg.DOCADriver.Enable = false
	return true
}

// ValidateDOCAHostDriverChoice requires an explicit choice when a single global
// driver policy cannot safely default across workers with different evidence.
// Discovery still saves this inventory so the owner can review and edit it.
func ValidateDOCAHostDriverChoice(cfg *config.LaunchKitConfig, present config.PathSet) error {
	if cfg == nil || present.Has("docaDriver.enable") {
		return nil
	}
	installed, total, membershipKnown := config.DOCAHostCoverage(cfg)
	if installed > 0 && !membershipKnown {
		return fmt.Errorf("host DOCA-HOST is installed on %d enumerated worker(s), but some inventory groups lack complete workerNodes; set docaDriver.enable explicitly after reviewing host/container driver ownership, or complete the worker inventory", installed)
	}
	if installed > 0 && installed < total {
		return fmt.Errorf("host DOCA-HOST is installed on %d of %d inventory workers; set docaDriver.enable explicitly after reviewing host/container driver ownership, or select a homogeneous inventory", installed, total)
	}
	return nil
}
