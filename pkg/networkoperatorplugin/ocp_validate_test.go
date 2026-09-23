// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestValidateOCPNetworkAttachmentChecksNamespaceAndResource(t *testing.T) {
	network := ocpTestObject("sriovnetwork.openshift.io/v1", "SriovNetwork", "sriov-operator", "network-a", map[string]interface{}{
		"spec": map[string]interface{}{"networkNamespace": "workloads", "resourceName": "resource-a"},
	})
	require.True(t, isOCPSriovNetwork(network))
	ctx := context.Background()
	state, _ := validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().Build(), network)
	require.Equal(t, crstate.StateInProgress, state)

	nad := ocpTestObject("k8s.cni.cncf.io/v1", "NetworkAttachmentDefinition", "workloads", "network-a", nil)
	nad.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/resourceName": "nvidia.com/resource-a"})
	state, reason := validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().WithObjects(nad).Build(), network)
	require.Equal(t, crstate.StateError, state)
	require.Contains(t, reason, "openshift.io/resource-a")

	nad.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/resourceName": "openshift.io/resource-a"})
	state, reason = validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().WithObjects(nad).Build(), network)
	require.Equal(t, crstate.StateSuccess, state)
	require.Contains(t, reason, "workloads/network-a")

	missingNamespace := network.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(missingNamespace.Object, "", "spec", "networkNamespace"))
	state, _ = validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().Build(), missingNamespace)
	require.Equal(t, crstate.StateError, state)
}
