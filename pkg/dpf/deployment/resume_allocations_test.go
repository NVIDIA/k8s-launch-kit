// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResumeAllocationPrefixesRequireExactCompleteGeometry(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*unstructured.Unstructured, *unstructured.Unstructured)
		pass bool
	}{
		{"empty in progress", func(_ *unstructured.Unstructured, p *unstructured.Unstructured) {
			unstructured.RemoveNestedField(p.Object, "status", "allocations")
		}, true},
		{"owned workload geometry", func(*unstructured.Unstructured, *unstructured.Unstructured) {}, true},
		{"loopback32", func(p, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, int64(32), "spec", "ipv4Network", "prefixSize")
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.1/32"}}, "status", "allocations")
		}, true},
		{"outside pool", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "203.0.113.0/29"}}, "status", "allocations")
		}, false},
		{"overbroad prefix", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.0/24"}}, "status", "allocations")
		}, false},
		{"noncanonical prefix", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.1/29"}}, "status", "allocations")
		}, false},
		{"duplicate node", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.0/29"}, map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.8/29"}}, "status", "allocations")
		}, false},
		{"overlapping allocations", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.0/29"}, map[string]any{"nodeName": "other-node", "prefix": "198.51.100.0/29"}}, "status", "allocations")
		}, false},
		{"malformed allocation", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{"bad"}, "status", "allocations")
		}, false},
		{"missing node", func(_ *unstructured.Unstructured, c *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(c.Object, []any{map[string]any{"prefix": "198.51.100.0/29"}}, "status", "allocations")
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"ipv4Network": map[string]any{"network": "198.51.100.0/24", "prefixSize": int64(29)}}}}
			pool := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"allocations": []any{map[string]any{"nodeName": "selected-node", "prefix": "198.51.100.0/29"}}}}}
			test.edit(parent, pool)
			before, _ := json.Marshal([]any{parent, pool})
			got, err := resumeAllocationPrefixes(parent, pool)
			if (err == nil) != test.pass {
				t.Fatalf("allocations=%v err=%v", got, err)
			}
			if test.name == "owned workload geometry" && !reflect.DeepEqual(got, map[string]string{"selected-node": "198.51.100.0/29"}) {
				t.Fatalf("allocation lost selected Node: %v", got)
			}
			after, _ := json.Marshal([]any{parent, pool})
			if string(before) != string(after) {
				t.Fatal("allocation validation mutated source")
			}
		})
	}
}
