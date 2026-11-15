// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

func tenantKubeconfigFixture(t *testing.T) clientapi.Config {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	return clientapi.Config{
		CurrentContext: "tenant",
		Clusters:       map[string]*clientapi.Cluster{"tenant": {Server: "https://192.0.2.20:30538", CertificateAuthorityData: ca}},
		AuthInfos:      map[string]*clientapi.AuthInfo{"admin": {Token: "tenant-secret-canary"}},
		Contexts:       map[string]*clientapi.Context{"tenant": {Cluster: "tenant", AuthInfo: "admin"}},
	}
}

func TestTenantCredentialsRejectAmbientAndOverriddenAccess(t *testing.T) {
	fixture := tenantKubeconfigFixture(t)
	for name, mutate := range map[string]func(*clientapi.Config){
		"exec": func(c *clientapi.Config) { c.AuthInfos["admin"].Exec = &clientapi.ExecConfig{Command: "must-not-run"} },
		"provider": func(c *clientapi.Config) {
			c.AuthInfos["admin"].AuthProvider = &clientapi.AuthProviderConfig{Name: "must-not-run"}
		},
		"token file":         func(c *clientapi.Config) { c.AuthInfos["admin"].TokenFile = "/must-not-read" },
		"certificate file":   func(c *clientapi.Config) { c.AuthInfos["admin"].ClientCertificate = "/must-not-read" },
		"key file":           func(c *clientapi.Config) { c.AuthInfos["admin"].ClientKey = "/must-not-read" },
		"CA file":            func(c *clientapi.Config) { c.Clusters["tenant"].CertificateAuthority = "/must-not-read" },
		"proxy":              func(c *clientapi.Config) { c.Clusters["tenant"].ProxyURL = "http://192.0.2.99:8080" },
		"TLS name":           func(c *clientapi.Config) { c.Clusters["tenant"].TLSServerName = "other.example" },
		"insecure":           func(c *clientapi.Config) { c.Clusters["tenant"].InsecureSkipTLSVerify = true },
		"missing CA":         func(c *clientapi.Config) { c.Clusters["tenant"].CertificateAuthorityData = nil },
		"different server":   func(c *clientapi.Config) { c.Clusters["tenant"].Server = "https://192.0.2.21:30538" },
		"different port":     func(c *clientapi.Config) { c.Clusters["tenant"].Server = "https://192.0.2.20:6443" },
		"basic auth":         func(c *clientapi.Config) { c.AuthInfos["admin"].Password = "tenant-secret-canary" },
		"impersonate":        func(c *clientapi.Config) { c.AuthInfos["admin"].Impersonate = "admin" },
		"impersonate groups": func(c *clientapi.Config) { c.AuthInfos["admin"].ImpersonateGroups = []string{"system:masters"} },
		"missing credential": func(c *clientapi.Config) { c.AuthInfos["admin"].Token = "" },
		"missing context":    func(c *clientapi.Config) { c.CurrentContext = "" },
		"extra cluster":      func(c *clientapi.Config) { c.Clusters["other"] = c.Clusters["tenant"].DeepCopy() },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := fixture.DeepCopy()
			mutate(cfg)
			data, err := clientcmd.Write(*cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tenantRESTConfig(data, "https://192.0.2.20:30538", time.Second)
			if err == nil {
				t.Fatal("unsafe tenant credentials were accepted")
			}
			if strings.Contains(err.Error(), "tenant-secret-canary") || strings.Contains(err.Error(), "/must-not-read") {
				t.Fatal("credential detail leaked into error")
			}
		})
	}
	data, err := clientcmd.Write(fixture)
	if err != nil {
		t.Fatal(err)
	}
	config, err := tenantRESTConfig(data, "https://192.0.2.20:30538", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "https://192.0.2.20:30538" || config.BearerToken != "tenant-secret-canary" || config.Timeout != time.Second || config.Proxy == nil {
		t.Fatal("validated tenant access was not retained")
	}
	proxy, err := config.Proxy(&http.Request{})
	if err != nil || proxy != nil {
		t.Fatal("tenant access inherited an ambient proxy")
	}
}

func TestTenantAPIOriginRejectsNonOriginsAndInvalidPorts(t *testing.T) {
	for _, endpoint := range []string{
		"http://192.0.2.20:30538", "https://192.0.2.20", "https://192.0.2.20:0", "https://192.0.2.20:65536",
		"https://user:secret@192.0.2.20:30538", "https://192.0.2.20:30538/path", "https://192.0.2.20:30538?",
		"https://192.0.2.20:30538?token=secret", "https://192.0.2.20:30538#other", "https://224.0.0.1:30538",
	} {
		if _, err := tenantAPIOrigin(endpoint); err == nil {
			t.Errorf("accepted invalid origin %q", endpoint)
		}
	}
}

func TestTenantHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/redirected" {
			t.Error("tenant transport followed a redirect")
			return
		}
		if r.Header.Get("Authorization") != "Bearer tenant-secret-canary" {
			t.Error("fixture did not exercise credential-bearing transport")
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := tenantHTTPClient(&rest.Config{Host: server.URL, Timeout: time.Second, BearerToken: "tenant-secret-canary", TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil || requests.Load() != 1 {
		t.Fatalf("redirect was not stopped: err=%v requests=%d", err, requests.Load())
	}
}

func TestTenantHTTPClientDisablesProxyWithoutMutatingConfig(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	var proxyCalls atomic.Int32
	config := &rest.Config{
		Host: server.URL, Timeout: time.Second,
		TLSClientConfig: rest.TLSClientConfig{CAData: ca},
		Proxy: func(*http.Request) (*url.URL, error) {
			proxyCalls.Add(1)
			return url.Parse("http://192.0.2.99:1")
		},
	}
	client, err := tenantHTTPClient(config)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || proxyCalls.Load() != 0 {
		t.Fatal("tenant transport used the caller's proxy")
	}
	proxy, err := config.Proxy(&http.Request{})
	if err != nil || proxy == nil || proxyCalls.Load() != 1 {
		t.Fatal("tenant transport mutated the caller's proxy configuration")
	}
}
