// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	pkgerrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperatorReadinessErrorHintsWhenManaged verifies the deploy-time operator
// check is augmented with how to apply the generated install artifacts.
func TestOperatorReadinessErrorHintsWhenManaged(t *testing.T) {
	cfg := &config.LaunchKitConfig{OCPOperatorInstall: &config.OCPOperatorInstallConfig{Manage: true}}
	cause := errors.New("OpenShift nvidia-network-operator is not installed in nvidia-network-operator")

	err := operatorReadinessError(cause, cfg)
	require.Error(t, err)
	var se *pkgerrors.StructuredError
	require.ErrorAs(t, err, &se)
	assert.Contains(t, se.Suggestion, "Apply the generated operator-install artifacts")
	assert.ErrorIs(t, err, cause)
}

// TestOperatorReadinessErrorUnchangedWhenUnmanaged verifies the error is returned
// as-is when --manage-operators did not generate install artifacts.
func TestOperatorReadinessErrorUnchangedWhenUnmanaged(t *testing.T) {
	cause := errors.New("boom")
	assert.Equal(t, cause, operatorReadinessError(cause, &config.LaunchKitConfig{}))
	assert.Nil(t, operatorReadinessError(nil, &config.LaunchKitConfig{OCPOperatorInstall: &config.OCPOperatorInstallConfig{Manage: true}}))
}
