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

package crstate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const spcxAPIVersion = spcxGroup + "/" + spcxVersionAlpha2

func spcxManifest(name, ns string) *unstructured.Unstructured {
	return makeUnstructured(spcxAPIVersion, spcxKindRailPoolConfig, ns, name, nil)
}

func TestSpectrumXValidator(t *testing.T) {
	cases := []struct {
		name       string
		syncStatus string
		reason     string
		want       CRState
	}{
		{"Succeeded→success", spcxSyncStatusSucceeded, "", StateSuccess},
		{"Failed→error", spcxSyncStatusFailed, "boom", StateError},
		{"InProgress→in-progress", spcxSyncStatusInProgress, "", StateInProgress},
		{"Unknown→in-progress", spcxSyncStatusUnknown, "", StateInProgress},
		{"empty→in-progress", "", "", StateInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := spcxManifest("rails", "ns")
			live := manifest.DeepCopy()
			if tc.syncStatus != "" {
				_ = unstructured.SetNestedField(live.Object, tc.syncStatus, "status", "syncStatus")
			}
			if tc.reason != "" {
				_ = unstructured.SetNestedField(live.Object, tc.reason, "status", "reason")
			}
			c := newClient(t, live)
			res, err := spectrumXRailPoolConfigValidator(context.Background(), c, manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.State)
		})
	}

	t.Run("not-found→not-deployed", func(t *testing.T) {
		manifest := spcxManifest("rails", "ns")
		c := newClient(t)
		res, err := spectrumXRailPoolConfigValidator(context.Background(), c, manifest)
		require.NoError(t, err)
		assert.Equal(t, StateNotDeployed, res.State)
	})

	t.Run("registered in default registry", func(t *testing.T) {
		r := NewDefault()
		manifest := spcxManifest("rails", "ns")
		live := manifest.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, spcxSyncStatusSucceeded, "status", "syncStatus")
		c := newClient(t, live)
		res, err := r.Validate(context.Background(), c, manifest)
		require.NoError(t, err)
		assert.Equal(t, StateSuccess, res.State)
	})
}
