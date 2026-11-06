// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// NativeClients resolves only explicit config bindings, never ambient kubeconfig.
type NativeClients struct{}

type nativeClient struct {
	dynamic dynamic.Interface
	mapper  meta.RESTMapper
	core    typedcore.CoreV1Interface
}

func (c *nativeClient) ServerVersion(ctx context.Context) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := c.core.RESTClient().Get().AbsPath("/version").Do(requestCtx).Raw()
	if err != nil {
		return "", fmt.Errorf("cannot query bound Kubernetes server version")
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if json.Unmarshal(data, &version) != nil || version.GitVersion == "" {
		return "", fmt.Errorf("kubernetes server version response is invalid")
	}
	return version.GitVersion, nil
}

func (NativeClients) Management(_ context.Context, g config.Generation) (Client, error) {
	if g.KubeconfigRef.File == "" || g.Context == "" {
		return nil, fmt.Errorf("explicit management kubeconfig and context are required")
	}
	st, err := os.Lstat(g.KubeconfigRef.File)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return nil, fmt.Errorf("management kubeconfig must be a bounded regular file")
	}
	data, err := os.ReadFile(g.KubeconfigRef.File)
	if err != nil {
		return nil, fmt.Errorf("cannot read explicit management kubeconfig")
	}
	return clientFromBytes(data, g.Context)
}

func (NativeClients) Tenant(ctx context.Context, management Client, binding config.ClusterBinding) (Client, error) {
	client, ok := management.(*nativeClient)
	if !ok {
		return nil, fmt.Errorf("native tenant resolution requires native management access")
	}
	cluster, err := client.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUCluster", binding.Namespace, binding.Name))
	if err != nil {
		return nil, err
	}
	if string(cluster.GetUID()) != binding.ResourceUID {
		return nil, fmt.Errorf("DPUCluster UID changed before tenant resolution")
	}
	secret := text(cluster, "spec", "kubeconfig")
	if secret == "" {
		return nil, fmt.Errorf("DPUCluster has no explicit kubeconfig Secret reference")
	}
	// super-admin.conf is the exact rc.1 controller contract; admin.conf is the
	// explicitly observed Kamaji compatibility key. Never scan arbitrary values.
	object, err := client.Get(ctx, ref(bundle.Management, "", "Secret", binding.Namespace, secret))
	if err != nil {
		return nil, fmt.Errorf("cannot read bound tenant kubeconfig Secret")
	}
	key := "super-admin.conf"
	if text(object, "data", key) == "" {
		key = "admin.conf"
	}
	data, err := client.secretData(ctx, binding.Namespace, secret, key)
	if err != nil {
		return nil, err
	}
	return clientFromBytes(data, "")
}

func clientFromBytes(data []byte, selectedContext string) (Client, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("cannot decode explicit kubeconfig")
	}
	if selectedContext == "" {
		selectedContext = cfg.CurrentContext
	}
	selected, exists := cfg.Contexts[selectedContext]
	if !exists {
		return nil, fmt.Errorf("explicit kubeconfig context is absent")
	}
	cluster, exists := cfg.Clusters[selected.Cluster]
	if !exists || cluster.Server == "" || cluster.InsecureSkipTLSVerify {
		return nil, fmt.Errorf("kubeconfig requires a verified TLS cluster binding")
	}
	server, err := url.Parse(cluster.Server)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return nil, fmt.Errorf("kubeconfig requires an explicit HTTPS API server origin")
	}
	auth, exists := cfg.AuthInfos[selected.AuthInfo]
	if !exists || auth.Exec != nil || auth.AuthProvider != nil {
		return nil, fmt.Errorf("kubeconfig external credential execution is unsupported by the native DPF slice")
	}
	// Tenant kubeconfig Secret data must be self-contained; file references could
	// accidentally resolve against workstation ambient paths.
	if auth.ClientCertificate != "" || auth.ClientKey != "" || auth.TokenFile != "" || cluster.CertificateAuthority != "" {
		return nil, fmt.Errorf("DPF kubeconfig must contain embedded credentials and CA data, not file references")
	}
	minimal := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{selected.Cluster: cluster}, AuthInfos: map[string]*clientcmdapi.AuthInfo{selected.AuthInfo: auth}, Contexts: map[string]*clientcmdapi.Context{selectedContext: selected}, CurrentContext: selectedContext}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(minimal, selectedContext, nil, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve explicit kubeconfig")
	}
	restConfig.Timeout = 30 * time.Second
	return newNativeClient(restConfig)
}

func newNativeClient(cfg *rest.Config) (Client, error) {
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cannot construct native dynamic client")
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cannot construct native discovery client")
	}
	streamConfig := rest.CopyConfig(cfg)
	streamConfig.Timeout = 0
	coreClient, err := typedcore.NewForConfig(streamConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot construct bound Kubernetes stream client")
	}
	return &nativeClient{dynamic: dynamicClient, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient)), core: coreClient}, nil
}

func (c *nativeClient) resource(reference bundle.ResourceRef, version string, allowAllNamespaces bool) (dynamic.ResourceInterface, error) {
	versions := []string{}
	if version != "" {
		versions = append(versions, version)
	}
	mapping, err := c.mapper.RESTMapping(schema.GroupKind{Group: reference.Group, Kind: reference.Kind}, versions...)
	if err != nil {
		return nil, err
	}
	resource := c.dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if reference.Namespace == "" && !allowAllNamespaces {
			return nil, fmt.Errorf("served API requires explicit namespace")
		}
		return resource.Namespace(reference.Namespace), nil
	}
	if reference.Namespace != "" {
		return nil, fmt.Errorf("served API reports cluster scope for a namespaced bundle identity")
	}
	return resource, nil
}

func (c *nativeClient) Get(ctx context.Context, reference bundle.ResourceRef) (*unstructured.Unstructured, error) {
	resource, err := c.resource(reference, "", false)
	if err != nil {
		return nil, err
	}
	return resource.Get(ctx, reference.Name, metav1.GetOptions{})
}

func (c *nativeClient) List(ctx context.Context, reference bundle.ResourceRef, selector string) ([]*unstructured.Unstructured, error) {
	resource, err := c.resource(reference, "", true)
	if err != nil {
		return nil, err
	}
	listed, err := resource.List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 1000})
	if err != nil {
		return nil, err
	}
	if listed.GetContinue() != "" {
		return nil, fmt.Errorf("DPF lifecycle list exceeds bounded membership limit")
	}
	items := make([]*unstructured.Unstructured, 0, len(listed.Items))
	for n := range listed.Items {
		items = append(items, listed.Items[n].DeepCopy())
	}
	return items, nil
}

func (c *nativeClient) Create(ctx context.Context, object *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	gvk := object.GroupVersionKind()
	resource, err := c.resource(ref("", gvk.Group, gvk.Kind, object.GetNamespace(), object.GetName()), gvk.Version, false)
	if err != nil {
		return nil, err
	}
	opts := metav1.CreateOptions{FieldManager: "k8s-launch-kit-dpf"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return resource.Create(ctx, object.DeepCopy(), opts)
}

func (c *nativeClient) secretData(ctx context.Context, namespace, name, key string) ([]byte, error) {
	secret, err := c.Get(ctx, ref(bundle.Management, "", "Secret", namespace, name))
	if err != nil {
		return nil, fmt.Errorf("cannot read required private Secret %s/%s", namespace, name)
	}
	raw := text(secret, "data", key)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(data) == 0 || len(data) > 4<<20 {
		return nil, fmt.Errorf("required Secret %s/%s key %s is missing or invalid", namespace, name, key)
	}
	return data, nil
}

func (c *nativeClient) SecretKeys(ctx context.Context, namespace, name string, keys []string) error {
	for _, key := range keys {
		if _, err := c.secretData(ctx, namespace, name, key); err != nil {
			return err
		}
	}
	return nil
}
