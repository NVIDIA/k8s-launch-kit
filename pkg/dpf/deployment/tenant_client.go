// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// Tenant credentials are read from the bound management cluster, never from a
// second caller-supplied kubeconfig. This interface also keeps source replacement
// tests independent of a live API server.
type tenantSourceReader interface {
	observeTenantSources(context.Context, TenantReadinessExpected) (tenantSources, error)
}

type tenantSources struct {
	observed TenantReadinessObserved
	secret   *corev1.Secret
}

type tenantClient struct {
	source        tenantSourceReader
	expected      TenantReadinessExpected
	dpuClusterUID string
	sourceDigest  [32]byte
	systemUID     string
	api           kubernetes.Interface
}

func newTenantClient(ctx context.Context, source tenantSourceReader, expected TenantReadinessExpected, dpuClusterUID string, timeout time.Duration) (*tenantClient, error) {
	if ctx == nil || source == nil || dpuClusterUID == "" || timeout <= 0 {
		return nil, fmt.Errorf("tenant access requires a bound source, DPUCluster UID and positive timeout")
	}
	// Freeze caller-owned maps. The retained intent must not change after source
	// binding has been established.
	expected.Labels = maps.Clone(expected.Labels)
	expected.NodeSelector = maps.Clone(expected.NodeSelector)
	sources, err := source.observeTenantSources(ctx, expected)
	if err != nil {
		return nil, err
	}
	digest, endpoint, data, err := validateTenantSources(expected, dpuClusterUID, sources)
	if err != nil {
		return nil, err
	}
	config, err := tenantRESTConfig(data, endpoint, timeout)
	if err != nil {
		return nil, err
	}
	transport, err := tenantHTTPClient(config)
	if err != nil {
		return nil, err
	}
	api, err := kubernetes.NewForConfigAndClient(config, transport)
	if err != nil {
		return nil, fmt.Errorf("cannot construct tenant API client")
	}
	client := &tenantClient{source: source, expected: expected, dpuClusterUID: dpuClusterUID, sourceDigest: digest, api: api}
	uid, err := client.observeIdentity(ctx)
	if err != nil {
		return nil, err
	}
	client.systemUID = uid
	// The API reads above are accepted only if their management source chain has
	// not changed while connecting to the tenant.
	if err := client.verifySources(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func validateTenantSources(expected TenantReadinessExpected, dcUID string, sources tenantSources) ([32]byte, string, []byte, error) {
	var empty [32]byte
	result := EvaluateTenantReadiness(expected, sources.observed)
	if !result.Ready {
		return empty, "", nil, fmt.Errorf("tenant source resources are not ready: %s", result.ReasonCode)
	}
	dc, tcp, svc := sources.observed.DPUCluster, sources.observed.TenantControlPlane, sources.observed.Service
	if dcUID == "" || string(dc.GetUID()) != dcUID {
		return empty, "", nil, fmt.Errorf("selected DPUCluster was replaced")
	}
	version, _, err := unstructured.NestedString(dc.Object, "status", "version")
	if err != nil || strings.TrimPrefix(version, "v") != strings.TrimPrefix(expected.KubernetesVersion, "v") {
		return empty, "", nil, fmt.Errorf("DPUCluster running version differs from the selected tenant version")
	}
	name, _, err := unstructured.NestedString(dc.Object, "spec", "kubeconfig")
	secret := sources.secret
	if err != nil || name != expected.Name+"-admin-kubeconfig" || secret == nil || secret.Name != name || secret.Namespace != expected.Namespace || secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return empty, "", nil, fmt.Errorf("tenant credential Secret has no stable matching identity")
	}
	// The pinned consumer requires no ownerReference. When Kamaji supplies one,
	// it must corroborate the exact TCP instead of contradicting the source chain.
	for _, owner := range secret.OwnerReferences {
		if owner.Kind == "TenantControlPlane" && (owner.APIVersion != tcp.GetAPIVersion() || owner.Name != tcp.GetName() || owner.UID != tcp.GetUID()) {
			return empty, "", nil, fmt.Errorf("tenant credential Secret has a conflicting owner")
		}
	}
	data := secret.Data["super-admin.conf"]
	if len(data) == 0 || len(data) > 1<<20 {
		return empty, "", nil, fmt.Errorf("tenant credential Secret lacks bounded super-admin.conf data")
	}
	port, _, err := unstructured.NestedInt64(tcp.Object, "spec", "networkProfile", "port")
	if err != nil || port < 1 || port > 65535 {
		return empty, "", nil, fmt.Errorf("tenant API port is unavailable")
	}
	endpoint := "https://" + net.JoinHostPort(expected.VIP, strconv.FormatInt(port, 10))
	// Only stable identities and intent are fingerprinted. Routine status and
	// resourceVersion updates must not invalidate an otherwise unchanged client.
	objects := []*unstructured.Unstructured{dc, tcp, svc}
	objects = append(objects, sources.observed.Keepalived...)
	bound := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		bound = append(bound, map[string]any{"apiVersion": object.GetAPIVersion(), "kind": object.GetKind(), "uid": string(object.GetUID()), "spec": object.Object["spec"], "data": object.Object["data"]})
	}
	binding := struct {
		Objects          []map[string]any
		SecretUID        string
		CredentialDigest [32]byte
	}{bound, string(secret.UID), sha256.Sum256(data)}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return empty, "", nil, fmt.Errorf("cannot bind tenant source identities")
	}
	return sha256.Sum256(encoded), endpoint, data, nil
}

func (c *tenantClient) verifySources(ctx context.Context) error {
	if c == nil || c.source == nil {
		return fmt.Errorf("tenant client is unavailable")
	}
	sources, err := c.source.observeTenantSources(ctx, c.expected)
	if err != nil {
		return err
	}
	digest, _, _, err := validateTenantSources(c.expected, c.dpuClusterUID, sources)
	if err != nil {
		return err
	}
	if digest != c.sourceDigest {
		return fmt.Errorf("tenant source resources or credentials changed; reopen tenant access")
	}
	return nil
}

func (c *tenantClient) observeIdentity(ctx context.Context) (string, error) {
	if c == nil || c.api == nil {
		return "", fmt.Errorf("tenant client is unavailable")
	}
	ns, err := c.api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", safeAPIError("read tenant identity", err)
	}
	if ns.UID == "" || ns.DeletionTimestamp != nil || ns.Status.Phase != corev1.NamespaceActive {
		return "", fmt.Errorf("tenant system namespace is not active with a stable identity")
	}
	data, err := c.api.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", safeAPIError("read tenant version", err)
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if json.Unmarshal(data, &version) != nil || strings.TrimPrefix(version.GitVersion, "v") != strings.TrimPrefix(c.expected.KubernetesVersion, "v") {
		return "", fmt.Errorf("tenant API version differs from the selected DPF tenant version")
	}
	return string(ns.UID), nil
}

func (c *tenantClient) VerifyIdentity(ctx context.Context) error {
	if err := c.verifySources(ctx); err != nil {
		return err
	}
	uid, err := c.observeIdentity(ctx)
	if err != nil {
		return err
	}
	if c.systemUID == "" || uid != c.systemUID {
		return fmt.Errorf("tenant API identity changed")
	}
	return c.verifySources(ctx)
}

// observeTenantSources reads only the fixed management resources created by the
// pinned DPF/Kamaji tenant controller. It does not expand the bundle's allowed
// render kinds or offer mutations of controller-owned companion resources.
func (c *APIClient) observeTenantSources(ctx context.Context, expected TenantReadinessExpected) (tenantSources, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return tenantSources{}, err
	}
	if err := validateTenantExpectation(expected); err != nil {
		return tenantSources{}, err
	}
	var sources tenantSources
	targets := []struct {
		gvr         schema.GroupVersionResource
		name        string
		destination **unstructured.Unstructured
	}{
		{schema.GroupVersionResource{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Resource: "dpuclusters"}, expected.Name, &sources.observed.DPUCluster},
		{schema.GroupVersionResource{Group: "kamaji.clastix.io", Version: "v1alpha1", Resource: "tenantcontrolplanes"}, expected.Name, &sources.observed.TenantControlPlane},
		{schema.GroupVersionResource{Version: "v1", Resource: "services"}, expected.Name, &sources.observed.Service},
	}
	for _, target := range targets {
		object, err := c.dynamic.Resource(target.gvr).Namespace(expected.Namespace).Get(ctx, target.name, metav1.GetOptions{})
		if err != nil {
			return tenantSources{}, safeAPIError("read tenant source", err)
		}
		*target.destination = object
	}
	for _, gvr := range []schema.GroupVersionResource{{Version: "v1", Resource: "configmaps"}, {Group: "apps", Version: "v1", Resource: "daemonsets"}} {
		object, err := c.dynamic.Resource(gvr).Namespace(expected.Namespace).Get(ctx, expected.Name+"-keepalived", metav1.GetOptions{})
		if err != nil {
			return tenantSources{}, safeAPIError("read tenant keepalived resource", err)
		}
		sources.observed.Keepalived = append(sources.observed.Keepalived, object)
	}
	secret, err := c.access.Client.CoreV1().Secrets(expected.Namespace).Get(ctx, expected.Name+"-admin-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return tenantSources{}, safeAPIError("read tenant credential Secret", err)
	}
	sources.secret = secret
	if err := c.VerifyIdentity(ctx); err != nil {
		return tenantSources{}, err
	}
	return sources, nil
}
