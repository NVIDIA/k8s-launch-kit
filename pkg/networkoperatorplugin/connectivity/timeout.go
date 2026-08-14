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

package connectivity

import "time"

const (
	automaticSetupTimeout        = 10 * time.Minute
	automaticSafetyMinimum       = 30 * time.Second
	matrixCleanupTimeout         = 30 * time.Second
	rdmaServerCleanupTimeout     = 3 * time.Second
	routeCheckTimeout            = 10 * time.Second
	nonRequiredCommandTimeout    = 5 * time.Second
	icmpCommandTimeout           = 5 * time.Second
	rpingCommandTimeout          = 30 * time.Second
	ibWriteBwCommandTimeout      = 45 * time.Second
	automaticSafetyMarginDivisor = 10
)

type timeoutBudget struct {
	Setup        time.Duration
	Tests        time.Duration
	SafetyMargin time.Duration
	Cleanup      time.Duration
	Total        time.Duration
	PlannedTests int
}

func (b timeoutBudget) executionTimeout() time.Duration {
	return b.Tests + b.SafetyMargin
}

// automaticTimeoutBudget mirrors the serial execution graph in RunMatrix.
// Route probes run before each selected RDMA family, RDMA commands are grouped
// by ordered pod pair, and ICMP tests run one-by-one. Keeping the calculation
// beside the command timeout constants makes the default deadline grow with
// the actual plan instead of relying on a fixed cluster-size assumption.
func automaticTimeoutBudget(plan MatrixPlan, checks []Check) timeoutBudget {
	checks = normalizeChecks(checks)
	budget := timeoutBudget{
		Setup:   automaticSetupTimeout,
		Cleanup: matrixCleanupTimeout,
	}

	for _, check := range checks {
		tests := testsForCheck(plan, check)
		budget.PlannedTests += len(tests)
		if len(tests) == 0 {
			continue
		}

		switch check {
		case CheckICMP:
			budget.Tests += requiredRouteBudget(tests)
			for _, test := range tests {
				budget.Tests += commandTimeoutFor(test, icmpCommandTimeout)
			}
		case CheckRPing:
			budget.Tests += requiredRouteBudget(tests)
			for _, batch := range groupRPingTests(tests) {
				budget.Tests += rdmaBatchTimeoutFor(batch) + rdmaServerCleanupTimeout
			}
		case CheckIBWriteBW, CheckGPUDirectDMABuf:
			budget.Tests += requiredRouteBudget(tests)
			for _, batch := range groupRPingTests(tests) {
				budget.Tests += ibWriteBwBatchTimeoutFor(batch) + rdmaServerCleanupTimeout
			}
		}
	}

	if budget.Tests > 0 {
		budget.SafetyMargin = budget.Tests / automaticSafetyMarginDivisor
		if budget.SafetyMargin < automaticSafetyMinimum {
			budget.SafetyMargin = automaticSafetyMinimum
		}
	}
	budget.Total = budget.Setup + budget.executionTimeout() + budget.Cleanup
	return budget
}

func testsForCheck(plan MatrixPlan, check Check) []PingTest {
	switch check {
	case CheckICMP:
		return append(append([]PingTest{}, plan.ICMPSameRail...), plan.ICMPCrossRail...)
	case CheckRPing:
		return append(append([]PingTest{}, plan.RDMASameRail...), plan.RDMACrossRail...)
	case CheckIBWriteBW:
		return append(append([]PingTest{}, plan.RDMABwSameRail...), plan.RDMABwCrossRail...)
	case CheckGPUDirectDMABuf:
		return append(append([]PingTest{}, plan.GPUDirectDMABufSameRail...), plan.GPUDirectDMABufCrossRail...)
	default:
		return nil
	}
}

func requiredRouteBudget(tests []PingTest) time.Duration {
	var budget time.Duration
	for _, test := range tests {
		if test.Expectation == "" || test.Expectation == ExpectRequired {
			budget += routeCheckTimeout
		}
	}
	return budget
}
