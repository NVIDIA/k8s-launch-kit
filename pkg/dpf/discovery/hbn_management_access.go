// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
)

// ObserveProvisioning uses the same explicit, snapshotted access as the cluster
// identity observation. It never loads ambient kubeconfig or changes resources.
func (m ManagementClient) ObserveProvisioning(ctx context.Context, namespace string) managementprobe.Snapshot {
	unavailable := func() managementprobe.Snapshot {
		return managementprobe.Snapshot{Namespace: namespace, Issues: []managementprobe.Issue{{Source: "management", Code: "CLIENT_UNAVAILABLE", Message: "Cannot construct DPF inspection clients from the selected management access."}}}
	}
	if m.REST == nil || m.Client == nil {
		return unavailable()
	}
	dynamicClient, err := dynamic.NewForConfig(m.REST)
	if err != nil {
		return unavailable()
	}
	extensionsClient, err := apiextensionsclient.NewForConfig(m.REST)
	if err != nil {
		return unavailable()
	}
	collector := managementprobe.Collector{Kubernetes: m.Client, Dynamic: dynamicClient, Extensions: extensionsClient}
	return collector.Collect(ctx, namespace)
}
