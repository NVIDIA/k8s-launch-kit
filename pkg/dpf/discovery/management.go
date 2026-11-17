// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Observe collects bounded management inventory using an existing access snapshot.
func Observe(ctx context.Context, m managementaccess.Client) (*inventory.Management, error) {
	result, err := m.ObserveIdentity(ctx)
	if err != nil {
		return nil, err
	}
	result.Nodes = []inventory.Node{}
	cursor := ""
	for {
		nodes, err := m.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 64, Continue: cursor})
		if err != nil {
			return nil, fmt.Errorf("management node inspection failed")
		}
		for _, n := range nodes.Items {
			node := inventory.Node{Labels: maps.Clone(n.Labels), Name: n.Name, UID: string(n.UID), Architecture: n.Status.NodeInfo.Architecture, OS: n.Status.NodeInfo.OperatingSystem, Unschedulable: n.Spec.Unschedulable, Addresses: []string{}}
			_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
			_, master := n.Labels["node-role.kubernetes.io/master"]
			node.ControlPlane = cp || master
			for _, condition := range n.Status.Conditions {
				if condition.Type == corev1.NodeReady {
					node.Ready = condition.Status == corev1.ConditionTrue
				}
			}
			for _, address := range n.Status.Addresses {
				node.Addresses = append(node.Addresses, address.Address)
			}
			slices.Sort(node.Addresses)
			result.Nodes = append(result.Nodes, node)
		}
		if len(result.Nodes) > 64 {
			return nil, fmt.Errorf("management node scope exceeds 64 nodes")
		}
		cursor = nodes.Continue
		if cursor == "" {
			break
		}
	}
	result.EvidenceRefs = append(result.EvidenceRefs, "management-nodes")
	result.Observations = map[string]inventory.Observation{}
	storage, storageErr := m.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{Limit: 256})
	storageFact := apiObservationFailure(storageErr)
	if storageErr == nil && storage.Continue != "" {
		storageFact = inventory.Observation{State: "unknown", ReasonCode: "API_COVERAGE_INCOMPLETE", Message: "StorageClass enumeration exceeded the bounded first page; not all entries were observed."}
	}
	if storageErr == nil && storage.Continue == "" {
		storageFact.State = "known"
		storageFact.ReasonCode = ""
		storageFact.Message = ""
		for _, s := range storage.Items {
			storageFact.Values = append(storageFact.Values, s.Name+"/"+s.Provisioner)
		}
		slices.Sort(storageFact.Values)
	}
	result.Observations["storageClasses"] = storageFact
	data, groupErr := m.Client.Discovery().RESTClient().Get().AbsPath("/apis").Do(ctx).Raw()
	var groups metav1.APIGroupList
	groupsFact := apiObservationFailure(groupErr)
	decodeErr := json.Unmarshal(data, &groups)
	if groupErr == nil && decodeErr != nil {
		groupsFact = inventory.Observation{State: "unknown", ReasonCode: "API_RESPONSE_INVALID", Message: "The API group discovery response is not valid APIGroupList JSON."}
	}
	if groupErr == nil && decodeErr == nil {
		groupsFact.State = "known"
		groupsFact.ReasonCode = ""
		groupsFact.Message = ""
		for _, g := range groups.Groups {
			if strings.HasSuffix(g.Name, "dpu.nvidia.com") {
				groupsFact.Values = append(groupsFact.Values, g.Name)
			}
		}
		slices.Sort(groupsFact.Values)
	}
	result.Observations["advertisedDPFAPIGroups"] = groupsFact
	slices.SortFunc(result.Nodes, func(a, b inventory.Node) int { return strings.Compare(a.UID, b.UID) })
	return result, nil
}

// NewEvidence binds API reads to the current collection rather than old success flags.
func NewEvidence(at time.Time) map[string]inventory.Evidence {
	return map[string]inventory.Evidence{"management-identity": {Kind: "kubernetes-api", InputRef: "/management", ResourceURI: "/api/v1/namespaces/kube-system", Property: "metadata.uid", ObservedAt: at, Vantage: "executor", Authenticated: true}, "management-nodes": {Kind: "kubernetes-api", InputRef: "/management", ResourceURI: "/api/v1/nodes", ObservedAt: at, Vantage: "executor", Authenticated: true}}
}

// apiObservationFailure preserves why optional API evidence is unavailable without raw errors.
func apiObservationFailure(err error) inventory.Observation {
	code, message := "API_INSPECTION_FAILED", "The Kubernetes API request failed; no complete observation was obtained."
	switch {
	case apierrors.IsForbidden(err):
		code, message = "API_ACCESS_DENIED", "The configured Kubernetes identity is forbidden from reading this resource."
	case apierrors.IsUnauthorized(err):
		code, message = "API_AUTHENTICATION_FAILED", "The Kubernetes API did not accept the configured credentials."
	case apierrors.IsNotFound(err):
		code, message = "API_RESOURCE_NOT_FOUND", "The requested Kubernetes API resource is not available."
	case errors.Is(err, context.Canceled):
		code, message = "API_CANCELLED", "The Kubernetes API inspection was cancelled."
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err):
		code, message = "API_TIMEOUT", "The Kubernetes API inspection timed out."
	}
	return inventory.Observation{State: "unknown", Values: []string{}, ReasonCode: code, Message: message}
}
