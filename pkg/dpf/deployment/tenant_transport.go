// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// tenantRESTConfig consumes only the controller's super-admin.conf bytes. The
// caller must first bind the DPUCluster, TCP, Service and Secret identities and
// derive expectedEndpoint from the verified VIP and allocated Service NodePort.
// Loading credentials alone does not establish tenant identity or readiness.
func tenantRESTConfig(data []byte, expectedEndpoint string, timeout time.Duration) (*rest.Config, error) {
	if len(data) == 0 || len(data) > 1<<20 || timeout <= 0 {
		return nil, fmt.Errorf("bounded tenant kubeconfig and positive request timeout are required")
	}
	expected, err := tenantAPIOrigin(expectedEndpoint)
	if err != nil {
		return nil, err
	}
	raw, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("cannot decode tenant kubeconfig")
	}
	if len(raw.Contexts) != 1 || len(raw.Clusters) != 1 || len(raw.AuthInfos) != 1 || raw.CurrentContext == "" {
		return nil, fmt.Errorf("tenant kubeconfig requires one explicit current context, cluster and credential")
	}
	selected := raw.Contexts[raw.CurrentContext]
	if selected == nil {
		return nil, fmt.Errorf("tenant kubeconfig current context is missing")
	}
	cluster, auth := raw.Clusters[selected.Cluster], raw.AuthInfos[selected.AuthInfo]
	if cluster == nil || auth == nil {
		return nil, fmt.Errorf("tenant kubeconfig context has missing cluster or credential references")
	}
	origin, err := tenantAPIOrigin(cluster.Server)
	if err != nil || origin != expected {
		return nil, fmt.Errorf("tenant kubeconfig server differs from the verified VIP and allocated API port")
	}
	if cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) == 0 || cluster.CertificateAuthority != "" || cluster.TLSServerName != "" || cluster.ProxyURL != "" {
		return nil, fmt.Errorf("tenant kubeconfig requires embedded CA trust without TLS, server-name or proxy overrides")
	}
	if auth.Exec != nil || auth.AuthProvider != nil || auth.Username != "" || auth.Password != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.TokenFile != "" {
		return nil, fmt.Errorf("tenant credentials must be embedded; plugins, basic authentication and file references are unsupported")
	}
	if auth.Impersonate != "" || auth.ImpersonateUID != "" || len(auth.ImpersonateGroups) != 0 || len(auth.ImpersonateUserExtra) != 0 {
		return nil, fmt.Errorf("tenant kubeconfig impersonation is unsupported")
	}
	certificate, key := len(auth.ClientCertificateData) > 0, len(auth.ClientKeyData) > 0
	if certificate != key || !certificate && auth.Token == "" || certificate && auth.Token != "" {
		return nil, fmt.Errorf("tenant kubeconfig requires one embedded certificate/key pair or bearer token")
	}
	// No loading rules or overrides: a Secret must not resolve local paths or
	// ambient kubeconfig. ClientConfig snapshots the validated embedded material.
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, nil, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve embedded tenant credentials")
	}
	config.Timeout = timeout
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	if _, err := rest.TLSConfigFor(config); err != nil {
		return nil, fmt.Errorf("tenant TLS credentials or trust are invalid")
	}
	return config, nil
}

func tenantAPIOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return "", fmt.Errorf("tenant API requires an explicit HTTPS IP origin and port")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || portErr != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("tenant API requires a unicast IP and valid explicit port")
	}
	return "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

// tenantHTTPClient must be shared by all clients for this tenant. In particular,
// never let a tenant API redirect cause another request with tenant credentials.
func tenantHTTPClient(config *rest.Config) (*http.Client, error) {
	if config == nil || config.Timeout <= 0 {
		return nil, fmt.Errorf("bounded tenant REST configuration is required")
	}
	// Keep the transport boundary independent of ambient proxy settings, even
	// when a caller constructs a config without tenantRESTConfig. Copy first so
	// constructing a client cannot mutate the retained caller configuration.
	transportConfig := rest.CopyConfig(config)
	transportConfig.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	client, err := rest.HTTPClientFor(transportConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot construct tenant HTTP transport")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("tenant API redirects are unsupported")
	}
	return client, nil
}
