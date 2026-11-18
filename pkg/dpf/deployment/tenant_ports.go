// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type tenantVerificationReader interface {
	VerifyIdentity(context.Context) error
	matchesTenantBinding(tenantSourceReader, controllercontract.TenantReadinessExpected, types.UID) bool
	matchesTenantTarget(controllercontract.TenantReadinessExpected, types.UID) bool
	clusterIdentity() types.UID
	expectation() controllercontract.TenantReadinessExpected
	getWorkloadChild(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
}
type tenantAllocationReader interface {
	tenantVerificationReader
	matchesTenantSource(tenantSourceReader) bool
	listCompleteNodes(context.Context) ([]unstructured.Unstructured, error)
}
type hbnRuntimeTenantReader interface {
	VerifyIdentity(context.Context) error
	listRuntimeObjects(context.Context, string, string) ([]appsv1.DaemonSet, []corev1.Pod, error)
	readPod(context.Context, string, string) (*corev1.Pod, error)
	checkTenantRuntimeReadPermissions(context.Context, string) []PreflightCheck
	checkTenantPodExecPermissions(context.Context, string, string) []PreflightCheck
}
type tenantBootstrapReader interface {
	VerifyIdentity(context.Context) error
	expectation() controllercontract.TenantReadinessExpected
	readNode(context.Context, string) (*corev1.Node, error)
	readSecret(context.Context, string, string) (*corev1.Secret, error)
	readConfigMap(context.Context, string, string) (*corev1.ConfigMap, error)
	bootstrapCredentialSource(context.Context) (string, []byte, error)
}

// The stage composition owns all tenant observations. Consumers receive the
// smaller ports above and never a Kubernetes client factory.
type tenantStageReader interface {
	tenantAllocationReader
	hbnRuntimeTenantReader
	tenantBootstrapReader
	systemIdentity() string
	operationSources(context.Context) (map[string]types.UID, error)
	readRuntimeCommand(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error)
}

func (c *tenantClient) matchesTenantBinding(source tenantSourceReader, expected controllercontract.TenantReadinessExpected, uid types.UID) bool {
	return c != nil && c.source == source && c.matchesTenantTarget(expected, uid)
}
func (c *tenantClient) matchesTenantSource(source tenantSourceReader) bool {
	return c != nil && c.source == source
}
func (c *tenantClient) matchesTenantTarget(expected controllercontract.TenantReadinessExpected, uid types.UID) bool {
	return c != nil && c.dpuClusterUID == string(uid) && reflect.DeepEqual(c.expected, expected)
}
func (c *tenantClient) clusterIdentity() types.UID { return types.UID(c.dpuClusterUID) }
func (c *tenantClient) expectation() controllercontract.TenantReadinessExpected {
	expected := c.expected
	expected.Labels = maps.Clone(expected.Labels)
	expected.NodeSelector = maps.Clone(expected.NodeSelector)
	return expected
}
func (c *tenantClient) systemIdentity() string { return c.systemUID }
func (c *tenantClient) listCompleteNodes(ctx context.Context) ([]unstructured.Unstructured, error) {
	if c == nil || c.api == nil {
		return nil, fmt.Errorf("tenant API is unavailable")
	}
	return listCompleteTenantNodes(ctx, c.api.CoreV1())
}
func (c *tenantClient) readPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.api.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}
func (c *tenantClient) readNode(ctx context.Context, name string) (*corev1.Node, error) {
	return c.api.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}
func (c *tenantClient) readSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return c.api.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}
func (c *tenantClient) readConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	return c.api.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}
func (c *tenantClient) bootstrapCredentialSource(ctx context.Context) (string, []byte, error) {
	sources, err := c.source.observeTenantSources(ctx, c.expected)
	if err != nil {
		return "", nil, err
	}
	digest, endpoint, data, err := validateTenantSources(c.rules, c.expected, c.dpuClusterUID, sources)
	if err != nil || digest != c.sourceDigest {
		return "", nil, fmt.Errorf("tenant credential source changed")
	}
	return endpoint, data, nil
}
func (c *tenantClient) readRuntimeCommand(ctx context.Context, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
	if c == nil || c.hbnExec == nil {
		return nil, fmt.Errorf("tenant runtime command transport is unavailable")
	}
	return c.hbnExec.read(ctx, namespace, pod, command, prefix)
}

var _ tenantStageReader = (*tenantClient)(nil)

type runtimeVerificationTenantReader interface {
	tenantVerificationReader
	hbnRuntimeTenantReader
}
