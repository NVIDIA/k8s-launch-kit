// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package managementaccess opens explicit DPF management credentials and observes
// fresh cluster identity without collecting discovery inventory.
package managementaccess

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"
)

// Client holds one explicit credential and TLS snapshot for management API reads.
// Open creates a new snapshot; ObserveIdentity always performs fresh remote reads.
type Client struct {
	Client    kubernetes.Interface
	REST      *rest.Config
	Namespace string
}

// Open snapshots bounded explicit kubeconfig, credential and TLS references.
// It neither uses ambient kubeconfig nor performs remote API requests. A nil
// limiter preserves client-go pacing; an explicit limiter changes only pacing.
func Open(r config.Request, limiter flowcontrol.RateLimiter) (Client, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: r.Kubeconfig}
	raw, err := loadLocalConfiguration(r.Kubeconfig)
	if err != nil {
		return Client{}, err
	}
	selected := r.Context
	if selected == "" {
		selected = raw.CurrentContext
		if raw.Contexts[selected] == nil && len(raw.Contexts) == 1 {
			for k := range raw.Contexts {
				selected = k
			}
		}
	}
	return openSelectedConfiguration(raw, selected, rules, r, limiter)
}

func loadLocalConfiguration(filename string) (*clientcmdapi.Config, error) {
	content, err := config.ReadReference(filename, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("invalid bounded management kubeconfig")
	}
	raw, err := clientcmd.Load(content)
	if err != nil {
		return nil, fmt.Errorf("cannot load explicit management kubeconfig")
	}
	for _, a := range raw.AuthInfos {
		if a == nil {
			return nil, fmt.Errorf("management kubeconfig contains an invalid auth entry")
		}
		a.LocationOfOrigin = filename
	}
	for _, c := range raw.Clusters {
		if c == nil {
			return nil, fmt.Errorf("management kubeconfig contains an invalid cluster entry")
		}
		c.LocationOfOrigin = filename
	}
	if err := clientcmd.ResolveLocalPaths(raw); err != nil {
		return nil, fmt.Errorf("cannot resolve kubeconfig file references")
	}
	return raw, nil
}

// ReferencedFiles enumerates every declared local auth/trust reference, including
// unused contexts. It reads only the bounded kubeconfig, never the referenced
// credentials, and neither runs plugins nor contacts the cluster.
func ReferencedFiles(filename string) ([]string, error) {
	raw, err := loadLocalConfiguration(filename)
	if err != nil {
		return nil, err
	}
	files := []string{filename}
	for _, auth := range raw.AuthInfos {
		if auth.Exec != nil || auth.AuthProvider != nil {
			return nil, fmt.Errorf("cannot enumerate local file references in kubeconfig authentication plugins")
		}
		for _, path := range []string{auth.ClientCertificate, auth.ClientKey, auth.TokenFile} {
			if path != "" {
				files = append(files, path)
			}
		}
	}
	for _, cluster := range raw.Clusters {
		if cluster.CertificateAuthority != "" {
			files = append(files, cluster.CertificateAuthority)
		}
	}
	sort.Strings(files)
	return files, nil
}

func openSelectedConfiguration(raw *clientcmdapi.Config, selected string, rules *clientcmd.ClientConfigLoadingRules, r config.Request, limiter flowcontrol.RateLimiter) (Client, error) {
	contextConfig := raw.Contexts[selected]
	if contextConfig == nil {
		return Client{}, fmt.Errorf("management context selection is required")
	}
	auth := raw.AuthInfos[contextConfig.AuthInfo]
	cluster := raw.Clusters[contextConfig.Cluster]
	if auth == nil || cluster == nil {
		return Client{}, fmt.Errorf("management context has missing auth/cluster")
	}
	if auth.Exec != nil || auth.AuthProvider != nil || auth.Username != "" || auth.Password != "" {
		return Client{}, fmt.Errorf("management kubeconfig requires certificate/key or bearer token; exec/auth-provider/basic authentication unsupported")
	}
	if auth.Token != "" && auth.TokenFile != "" || len(auth.ClientCertificateData) > 0 && auth.ClientCertificate != "" || len(auth.ClientKeyData) > 0 && auth.ClientKey != "" || len(cluster.CertificateAuthorityData) > 0 && cluster.CertificateAuthority != "" {
		return Client{}, fmt.Errorf("management auth/trust references must use exactly one file or embedded value")
	}
	hasToken := auth.Token != "" || auth.TokenFile != ""
	hasCertificate := (auth.ClientCertificate != "" || len(auth.ClientCertificateData) > 0) && (auth.ClientKey != "" || len(auth.ClientKeyData) > 0)
	if !hasToken && !hasCertificate {
		return Client{}, fmt.Errorf("management certificate/key or bearer token is required")
	}
	u, err := url.Parse(cluster.Server)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cluster.InsecureSkipTLSVerify {
		return Client{}, fmt.Errorf("management server requires verified HTTPS without userinfo/query/fragment")
	}
	for _, reference := range []struct {
		file *string
		data *[]byte
	}{{&auth.ClientCertificate, &auth.ClientCertificateData}, {&auth.ClientKey, &auth.ClientKeyData}, {&cluster.CertificateAuthority, &cluster.CertificateAuthorityData}} {
		if *reference.file != "" && len(*reference.data) == 0 {
			b, e := config.ReadReference(*reference.file, 1<<20)
			if e != nil {
				return Client{}, fmt.Errorf("invalid bounded management auth/trust reference")
			}
			*reference.data = b
			*reference.file = ""
		}
	}
	if auth.TokenFile != "" && auth.Token == "" {
		b, e := config.ReadReference(auth.TokenFile, 64<<10)
		if e != nil {
			return Client{}, fmt.Errorf("invalid bounded token reference")
		}
		auth.Token = strings.TrimSpace(string(b))
		if auth.Token == "" {
			return Client{}, fmt.Errorf("empty management token reference")
		}
		auth.TokenFile = ""
	}
	loader := clientcmd.NewNonInteractiveClientConfig(*raw, selected, &clientcmd.ConfigOverrides{}, rules)
	rc, err := loader.ClientConfig()
	if err != nil {
		return Client{}, fmt.Errorf("cannot resolve management certificate/token configuration")
	}
	rc.Timeout = r.RequestTimeout
	rc.CAFile = ""
	if limiter != nil {
		rc.RateLimiter = limiter
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return Client{}, fmt.Errorf("cannot construct management client")
	}
	ns := r.Namespace
	if ns == "" {
		ns = contextConfig.Namespace
	}
	if ns == "" {
		ns = "default"
	}
	return Client{Client: client, REST: rc, Namespace: ns}, nil
}

// ObserveIdentity reads only the authenticated cluster identity and API version.
// Deploy callers use it to refresh the binding before effects without collecting
// unrelated inventory or resolving credentials again.
func (m Client) ObserveIdentity(ctx context.Context) (*inventory.Management, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.Client == nil || m.REST == nil {
		return nil, fmt.Errorf("management client is unavailable")
	}
	ns, err := m.Client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("management cluster identity read failed")
	}
	if ns.UID == "" {
		return nil, fmt.Errorf("management identity UID is empty")
	}
	// REST request honors caller cancellation, unlike ServerVersion's context-free API.
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	data, err := m.Client.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil || json.Unmarshal(data, &version) != nil || version.GitVersion == "" {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("management version read failed")
	}
	result := &inventory.Management{APIEndpoint: m.REST.Host, SystemNamespaceUID: string(ns.UID), KubernetesVersion: version.GitVersion, Namespace: m.Namespace, TrustDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(m.REST.CAData)), EvidenceRefs: []string{"management-identity"}}
	if len(m.REST.CAData) == 0 {
		result.TrustDigest = "system-roots"
	}
	if m.REST.ServerName != "" {
		result.TrustDigest = inventory.Digest("management-trust-v1", []string{result.TrustDigest, m.REST.ServerName})
	}
	return result, nil
}
