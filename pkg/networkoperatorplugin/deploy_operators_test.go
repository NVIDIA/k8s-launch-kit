// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	pkgerrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOperatorNamespaceDoc = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: nvidia-network-operator\n"
	testOperatorGroupDoc     = "apiVersion: operators.coreos.com/v1\nkind: OperatorGroup\nmetadata:\n  name: nvidia-network-operator\n  namespace: nvidia-network-operator\nspec:\n  targetNamespaces: [\"nvidia-network-operator\"]\n"
	testSubscriptionDoc      = "apiVersion: operators.coreos.com/v1alpha1\nkind: Subscription\nmetadata:\n  name: nvidia-network-operator\n  namespace: nvidia-network-operator\nspec:\n  name: nvidia-network-operator\n"
	testNicClusterPolicyDoc  = "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: nic-cluster-policy\n"
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

// TestPrepareDeploymentObjectsExcludesOperatorInstall verifies the OLM install
// artifacts are classified apart from the apply loop, not into selected.others.
func TestPrepareDeploymentObjectsExcludesOperatorInstall(t *testing.T) {
	artifacts, err := bundle.FromFiles([]bundle.File{
		{Name: "00-operator-namespaces.yaml", Content: testOperatorNamespaceDoc},
		{Name: "00-operatorgroups.yaml", Content: testOperatorGroupDoc},
		{Name: "00-subscriptions.yaml", Content: testSubscriptionDoc},
		{Name: "10-nicclusterpolicy.yaml", Content: testNicClusterPolicyDoc},
	})
	require.NoError(t, err)

	selected, err := prepareDeploymentObjects(artifacts, true)
	require.NoError(t, err)
	assert.Len(t, selected.operatorInstall, 3, "install artifacts are classified separately")
	assert.Empty(t, selected.others, "install artifacts do not enter the apply loop")
	require.NotNil(t, selected.ncp)
}

// TestValidateBundleSkipsOperatorInstall verifies install artifacts are not
// validated as deployed state, since deploy does not apply them.
func TestValidateBundleSkipsOperatorInstall(t *testing.T) {
	artifacts, err := bundle.FromFiles([]bundle.File{
		{Name: "00-operator-namespaces.yaml", Content: testOperatorNamespaceDoc},
		{Name: "00-operatorgroups.yaml", Content: testOperatorGroupDoc},
		{Name: "00-subscriptions.yaml", Content: testSubscriptionDoc},
	})
	require.NoError(t, err)

	// A nil client also proves these artifacts are skipped before live reads.
	results, err := ValidateBundle(context.Background(), nil, artifacts, nil, hostplatform.OpenShift)
	require.NoError(t, err)
	assert.Empty(t, results, "install artifacts are not part of validated deployed state")
}
