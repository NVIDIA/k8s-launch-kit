// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import "testing"

func TestHBNRuntimePinnedMultusAnnotation(t *testing.T) {
	// DPF fc4974ed uses Multus v4.0.2: CNIArgs has no omitempty, even when
	// nil. Compare literal wire bytes, independently of our App fixture.
	annotation, err := compileHBNNetworkAnnotation(`[{"name":"iprequest","interface":"ip_lo","cni-args":{"poolNames":["hbn-loopback"],"poolType":"cidrpool"}}]`, []hbnNetworkSelection{{Name: "mybrhbn", Interface: "p0_if"}})
	want := `[{"name":"invalid-network","namespace":"invalid-namespace","interface":"invalid-interface","cni-args":null},{"name":"iprequest","interface":"ip_lo","cni-args":{"poolNames":["hbn-loopback"],"poolType":"cidrpool"}},{"name":"mybrhbn","interface":"p0_if","cni-args":null}]`
	if err != nil || annotation != want {
		t.Fatalf("annotation=%s want=%s err=%v", annotation, want, err)
	}
}
func TestHBNRuntimeMergePreservesExplicitValues(t *testing.T) {
	dst := map[string]any{"enabled": false, "number": int64(0), "nullable": nil, "nested": map[string]any{"user": "explicit"}}
	src := map[string]any{"enabled": true, "number": int64(7), "nullable": "default", "nested": map[string]any{"user": "default", "other": "retained"}}
	result := mergeHBNValues(dst, src)
	if result["enabled"] != false || result["number"] != int64(0) || result["nullable"] != nil || result["nested"].(map[string]any)["user"] != "explicit" || result["nested"].(map[string]any)["other"] != "retained" {
		t.Fatal("lost explicit override or recursive template default")
	}
	result["nested"].(map[string]any)["user"] = "mutated"
	if dst["nested"].(map[string]any)["user"] != "explicit" || src["nested"].(map[string]any)["user"] != "default" {
		t.Fatal("merge mutated retained input")
	}
}
