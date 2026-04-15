// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package networkoperatorplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSniffKind(t *testing.T) {
	t.Run("NicClusterPolicy", func(t *testing.T) {
		doc := []byte(`apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
metadata:
  name: nic-cluster-policy`)
		assert.Equal(t, "NicClusterPolicy", sniffKind(doc))
	})

	t.Run("NicNodePolicy", func(t *testing.T) {
		doc := []byte(`apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: pool-a`)
		assert.Equal(t, "NicNodePolicy", sniffKind(doc))
	})

	t.Run("other kind", func(t *testing.T) {
		doc := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config`)
		assert.Equal(t, "ConfigMap", sniffKind(doc))
	})

	t.Run("invalid YAML returns empty", func(t *testing.T) {
		assert.Equal(t, "", sniffKind([]byte("not: [valid: yaml: {")))
	})

	t.Run("empty returns empty", func(t *testing.T) {
		assert.Equal(t, "", sniffKind([]byte("")))
	})
}

func TestContainsNicClusterPolicyKind(t *testing.T) {
	ncp := []byte(`kind: NicClusterPolicy`)
	nnp := []byte(`kind: NicNodePolicy`)
	other := []byte(`kind: ConfigMap`)

	assert.True(t, containsNicClusterPolicyKind(ncp))
	assert.False(t, containsNicClusterPolicyKind(nnp))
	assert.False(t, containsNicClusterPolicyKind(other))
}

func TestContainsNicNodePolicyKind(t *testing.T) {
	ncp := []byte(`kind: NicClusterPolicy`)
	nnp := []byte(`kind: NicNodePolicy`)
	other := []byte(`kind: ConfigMap`)

	assert.False(t, containsNicNodePolicyKind(ncp))
	assert.True(t, containsNicNodePolicyKind(nnp))
	assert.False(t, containsNicNodePolicyKind(other))
}

func TestSplitYAMLDocuments(t *testing.T) {
	t.Run("separates NCP and NNP docs", func(t *testing.T) {
		input := `---
kind: NicClusterPolicy
metadata:
  name: nic-cluster-policy
---
kind: NicNodePolicy
metadata:
  name: pool-a
---
kind: IPPool
metadata:
  name: my-pool`

		docs := splitYAMLDocuments(input)
		assert.Len(t, docs, 3)

		ncpCount, nnpCount, otherCount := 0, 0, 0
		for _, doc := range docs {
			b := []byte(doc)
			if containsNicClusterPolicyKind(b) {
				ncpCount++
			} else if containsNicNodePolicyKind(b) {
				nnpCount++
			} else {
				otherCount++
			}
		}
		assert.Equal(t, 1, ncpCount)
		assert.Equal(t, 1, nnpCount)
		assert.Equal(t, 1, otherCount)
	})

	t.Run("multiple NNP docs", func(t *testing.T) {
		input := `---
kind: NicNodePolicy
metadata:
  name: pool-a
---
kind: NicNodePolicy
metadata:
  name: pool-b`

		docs := splitYAMLDocuments(input)
		assert.Len(t, docs, 2)
		for _, doc := range docs {
			assert.True(t, containsNicNodePolicyKind([]byte(doc)))
		}
	})
}
