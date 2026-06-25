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

package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseModuleList(t *testing.T) {
	exclude := []string{"mlx5_core", "mlx5_ib", "ib_umad", "ib_uverbs", "ib_ipoib", "rdma_cm", "rdma_ucm", "ib_core", "ib_cm"}

	t.Run("filters out target modules", func(t *testing.T) {
		output := "iw_cm\nmlx5_core\nib_core\nnfsrdma\n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm", "nfsrdma"}, result)
	})

	t.Run("empty output returns nil", func(t *testing.T) {
		result := parseModuleList("", exclude)
		assert.Nil(t, result)
	})

	t.Run("only target modules returns nil", func(t *testing.T) {
		result := parseModuleList("mlx5_core\nib_core\n", exclude)
		assert.Nil(t, result)
	})

	t.Run("handles whitespace and blank lines", func(t *testing.T) {
		output := "  iw_cm  \n\n  xprtrdma\n  \n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm", "xprtrdma"}, result)
	})

	t.Run("deduplicates", func(t *testing.T) {
		output := "iw_cm\niw_cm\niw_cm\n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm"}, result)
	})
}
