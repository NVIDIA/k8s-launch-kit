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

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAutomaticTimeoutBudgetMirrorsSelectedExecutionPlan(t *testing.T) {
	tests := []PingTest{
		{SrcPod: "pod-a", DstPod: "pod-b", Expectation: ExpectRequired},
		{SrcPod: "pod-a", DstPod: "pod-b", Expectation: ExpectObserve},
		{SrcPod: "pod-b", DstPod: "pod-a", Expectation: ExpectForbidden},
	}
	plan := MatrixPlan{
		ICMPSameRail:            append([]PingTest(nil), tests...),
		RDMASameRail:            append([]PingTest(nil), tests...),
		RDMABwSameRail:          append([]PingTest(nil), tests...),
		GPUDirectDMABufSameRail: append([]PingTest(nil), tests...),
	}

	budget := automaticTimeoutBudget(plan, []Check{
		CheckRPing,
		CheckIBWriteBW,
		CheckGPUDirectDMABuf,
		CheckICMP,
	})

	assert.Equal(t, 12, budget.PlannedTests)
	assert.Equal(t, automaticSetupTimeout, budget.Setup)
	assert.Equal(t, 5*time.Minute+51*time.Second, budget.Tests)
	assert.Equal(t, 35*time.Second+100*time.Millisecond, budget.SafetyMargin)
	assert.Equal(t, 6*time.Minute+26*time.Second+100*time.Millisecond, budget.executionTimeout())
	assert.Equal(t, matrixCleanupTimeout, budget.Cleanup)
	assert.Equal(t, 16*time.Minute+56*time.Second+100*time.Millisecond, budget.Total)
}

func TestAutomaticTimeoutBudgetIncludesOnlySelectedChecks(t *testing.T) {
	plan := MatrixPlan{
		ICMPSameRail: []PingTest{
			{SrcPod: "pod-a", DstPod: "pod-b", Expectation: ExpectRequired},
			{SrcPod: "pod-b", DstPod: "pod-a", Expectation: ExpectObserve},
		},
		RDMASameRail: []PingTest{
			{SrcPod: "pod-a", DstPod: "pod-b", Expectation: ExpectRequired},
		},
	}

	budget := automaticTimeoutBudget(plan, []Check{CheckICMP})

	assert.Equal(t, 2, budget.PlannedTests)
	assert.Equal(t, 20*time.Second, budget.Tests)
	assert.Equal(t, automaticSafetyMinimum, budget.SafetyMargin)
	assert.Equal(t, automaticSetupTimeout+50*time.Second+matrixCleanupTimeout, budget.Total)
}

func TestAutomaticTimeoutBudgetForThreeNodeFourRailQuickMatrix(t *testing.T) {
	pairs := [][2]string{
		{"pod-a", "pod-b"},
		{"pod-a", "pod-c"},
		{"pod-b", "pod-a"},
		{"pod-b", "pod-c"},
		{"pod-c", "pod-a"},
		{"pod-c", "pod-b"},
	}
	tests := make([]PingTest, 0, 36)
	for _, pair := range pairs {
		for range 4 {
			tests = append(tests, PingTest{
				SrcPod: pair[0], DstPod: pair[1], Expectation: ExpectRequired,
			})
		}
	}
	for range 12 {
		tests = append(tests, PingTest{
			SrcPod: "pod-a", DstPod: "pod-b", Expectation: ExpectRequired,
		})
	}
	plan := MatrixPlan{
		ICMPSameRail:            append([]PingTest(nil), tests...),
		RDMASameRail:            append([]PingTest(nil), tests...),
		RDMABwSameRail:          append([]PingTest(nil), tests...),
		GPUDirectDMABufSameRail: append([]PingTest(nil), tests...),
	}

	budget := automaticTimeoutBudget(plan, []Check{
		CheckRPing,
		CheckIBWriteBW,
		CheckGPUDirectDMABuf,
		CheckICMP,
	})

	assert.Equal(t, 144, budget.PlannedTests)
	assert.Equal(t, 2*time.Hour+10*time.Minute+24*time.Second, budget.Total)
}

func TestAutomaticTimeoutBudgetUsesDefaultChecksAndNoSafetyForEmptyPlan(t *testing.T) {
	budget := automaticTimeoutBudget(MatrixPlan{}, nil)

	assert.Zero(t, budget.PlannedTests)
	assert.Zero(t, budget.Tests)
	assert.Zero(t, budget.SafetyMargin)
	assert.Zero(t, budget.executionTimeout())
	assert.Equal(t, automaticSetupTimeout+matrixCleanupTimeout, budget.Total)
}

func TestRequiredRouteBudgetTreatsEmptyExpectationAsRequired(t *testing.T) {
	tests := []PingTest{
		{Expectation: ""},
		{Expectation: ExpectRequired},
		{Expectation: ExpectObserve},
		{Expectation: ExpectForbidden},
	}

	assert.Equal(t, 2*routeCheckTimeout, requiredRouteBudget(tests))
}
