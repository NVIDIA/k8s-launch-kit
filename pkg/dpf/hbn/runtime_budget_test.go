// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbn

import (
	"math"
	"testing"
	"time"
)

func TestMinimumRuntimeTrafficDuration(t *testing.T) {
	for _, tc := range []struct {
		hosts int
		want  time.Duration
	}{
		{1, 0}, {2, 8 * time.Second}, {42, 6888 * time.Second}, {43, 7224 * time.Second}, {44, 7568 * time.Second}, {256, 261120 * time.Second},
	} {
		got, err := MinimumRuntimeTrafficDuration(tc.hosts)
		if err != nil || got != tc.want {
			t.Fatalf("hosts=%d duration=%s error=%v want=%s", tc.hosts, got, err, tc.want)
		}
	}
	for _, hosts := range []int{-1, 0, 257, math.MaxInt} {
		if _, err := MinimumRuntimeTrafficDuration(hosts); err == nil {
			t.Fatalf("invalid count %d accepted", hosts)
		}
	}
}

func TestRuntimeBudgetRejectsOnlyImpossiblePacketSpacing(t *testing.T) {
	for _, tc := range []struct {
		hosts   int
		budget  time.Duration
		allowed bool
	}{
		{1, time.Nanosecond, true}, {1, 0, false}, {2, 8 * time.Second, false}, {2, 8*time.Second + time.Nanosecond, true},
		{42, MaxNativeDeploymentDuration, true}, {43, MaxNativeDeploymentDuration, false}, {44, MaxNativeDeploymentDuration, false},
		{2, MaxNativeDeploymentDuration + time.Nanosecond, false}, {2, -time.Second, false}, {math.MaxInt, MaxNativeDeploymentDuration, false},
	} {
		err := ValidateRuntimeTrafficBudget(tc.hosts, tc.budget)
		if (err == nil) != tc.allowed {
			t.Fatalf("hosts=%d budget=%s allowed=%t error=%v", tc.hosts, tc.budget, tc.allowed, err)
		}
	}
}
