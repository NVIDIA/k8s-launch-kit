// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbn

import (
	"fmt"
	"time"
)

// The native acceptance protocol measures ordinary and DF traffic for every
// directed host pair, serially. Keep its packet spacing explicit at execution.
const (
	RuntimePacketCount          = 3
	RuntimePingInterval         = time.Second
	MaxNativeDeploymentDuration = 2 * time.Hour
)

// MinimumRuntimeTrafficDuration counts only packet spacing on a successful
// complete matrix. SSH, API fences, provisioning and every other operation add
// time. The native profile requires one selected DPU per physical host.
func MinimumRuntimeTrafficDuration(hostCount int) (time.Duration, error) {
	if hostCount < 1 || hostCount > maxDevices {
		return 0, fmt.Errorf("native HBN runtime requires 1 to %d selected physical hosts", maxDevices)
	}
	// Bound before multiplication, including for caller-supplied extreme ints.
	pairs := time.Duration(hostCount) * time.Duration(hostCount-1)
	return pairs * 2 * (RuntimePacketCount - 1) * RuntimePingInterval, nil
}

// ValidateRuntimeTrafficBudget rejects a mathematically impossible run. A pass
// is not an estimate or assurance that the remaining workflow will finish.
func ValidateRuntimeTrafficBudget(hostCount int, available time.Duration) error {
	minimum, err := MinimumRuntimeTrafficDuration(hostCount)
	if err != nil {
		return err
	}
	if available <= 0 || available > MaxNativeDeploymentDuration {
		return fmt.Errorf("native HBN deployment requires a positive budget no greater than %s", MaxNativeDeploymentDuration)
	}
	if minimum >= available {
		return fmt.Errorf("%d selected physical hosts require at least %s for ordinary/DF packet spacing alone; available %s must also cover provisioning, SSH and API checks", hostCount, minimum, available)
	}
	return nil
}
