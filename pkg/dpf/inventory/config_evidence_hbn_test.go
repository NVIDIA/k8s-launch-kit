// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = ginkgo.Describe("HBN management config diagnostics", func() {
	ginkgo.It("preserves evidence-like node selector keys as label data", func() {
		selector := map[string]string{"evidenceRef": "kept-label-value", "evidence": "another-label"}
		in := Inventory{
			FormatVersion: FormatVersion,
			HBN: &HBNInventory{Management: &HBNManagement{TenantClusters: []HBNManagementCluster{{
				Name: "tenant", Namespace: "tenant-ns", NodeSelector: &selector,
			}}}},
		}
		data, err := yaml.Marshal(in)
		Expect(err).NotTo(HaveOccurred())

		var encoded map[string]any
		Expect(yaml.Unmarshal(data, &encoded)).To(Succeed())
		hbnManagement := evidenceMapAt(encoded, "hbn", "management")
		tenantCluster := hbnManagement["tenantClusters"].([]any)[0].(map[string]any)
		nodeSelector := evidenceMapAt(tenantCluster, "nodeSelector")
		Expect(nodeSelector).To(HaveKeyWithValue("evidenceRef", "kept-label-value"))
		Expect(nodeSelector).To(HaveKeyWithValue("evidence", "another-label"))

		var restored Inventory
		Expect(yaml.Unmarshal(data, &restored)).To(Succeed())
		Expect(*restored.HBN.Management.TenantClusters[0].NodeSelector).To(Equal(selector))
	})
})

func evidenceMapAt(root map[string]any, path ...string) map[string]any {
	for _, key := range path {
		value, ok := root[key].(map[string]any)
		Expect(ok).To(BeTrue(), "expected mapping at %q", key)
		root = value
	}
	return root
}
