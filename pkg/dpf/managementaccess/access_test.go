// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementaccess_test

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"
)

func TestManagementAccess(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPF management access")
}

func writeKubeconfig(dir string, cluster *clientapi.Cluster, auth *clientapi.AuthInfo) string {
	GinkgoHelper()
	raw := clientapi.Config{CurrentContext: "management", Clusters: map[string]*clientapi.Cluster{"cluster": cluster}, AuthInfos: map[string]*clientapi.AuthInfo{"reader": auth}, Contexts: map[string]*clientapi.Context{"management": {Cluster: "cluster", AuthInfo: "reader", Namespace: "context-namespace"}}}
	data, err := clientcmd.Write(raw)
	Expect(err).NotTo(HaveOccurred())
	filename := filepath.Join(dir, "kubeconfig")
	Expect(os.WriteFile(filename, data, 0600)).To(Succeed())
	return filename
}

var _ = Describe("explicit management access snapshot", func() {
	It("selects only request pacing before native client construction and preserves nil defaults", func() {
		var requests atomic.Int32
		var wrongAuthentication atomic.Bool
		var uid atomic.Value
		uid.Store("cluster-one")
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") != "Bearer constructor-test-token" {
				wrongAuthentication.Store(true)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/prefix/api/v1/namespaces/kube-system":
				fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":%q}}`, uid.Load())
			case "/prefix/version":
				fmt.Fprint(w, `{"gitVersion":"v1.35.0"}`)
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		}))
		defer server.Close()
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		filename := writeKubeconfig(GinkgoT().TempDir(), &clientapi.Cluster{Server: server.URL + "/prefix", CertificateAuthorityData: ca}, &clientapi.AuthInfo{Token: "constructor-test-token"})
		request := config.Request{Kubeconfig: filename, RequestTimeout: time.Second}
		native, err := managementaccess.Open(request, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(native.REST.RateLimiter).To(BeNil())
		Expect(native.REST.QPS).To(BeZero())
		Expect(native.REST.Burst).To(BeZero())
		Expect(native.Client.CoreV1().RESTClient().GetRateLimiter().QPS()).To(Equal(rest.DefaultQPS))
		limiter := &constructorRateLimiter{RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()}
		selected, err := managementaccess.Open(request, limiter)
		Expect(err).NotTo(HaveOccurred())
		Expect(requests.Load()).To(BeZero())
		Expect(selected.REST.RateLimiter).To(BeIdenticalTo(limiter))
		Expect(selected.Client.CoreV1().RESTClient().GetRateLimiter()).To(BeIdenticalTo(limiter))
		Expect(selected.Client.Discovery().RESTClient().GetRateLimiter()).To(BeIdenticalTo(limiter))
		Expect(selected.REST.Host).To(Equal(native.REST.Host))
		Expect(selected.REST.TLSClientConfig).To(Equal(native.REST.TLSClientConfig))
		Expect(selected.REST.BearerToken).To(Equal(native.REST.BearerToken))
		Expect(selected.REST.Timeout).To(Equal(native.REST.Timeout))
		Expect(selected.REST.QPS).To(Equal(native.REST.QPS))
		Expect(selected.REST.Burst).To(Equal(native.REST.Burst))
		for _, expectedUID := range []string{"cluster-one", "cluster-two"} {
			uid.Store(expectedUID)
			identity, err := selected.ObserveIdentity(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.SystemNamespaceUID).To(Equal(expectedUID))
		}
		Expect(limiter.waits.Load()).To(Equal(int32(4)))
		Expect(requests.Load()).To(Equal(int32(4)))
		_, err = native.ObserveIdentity(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(requests.Load()).To(Equal(int32(6)))
		Expect(limiter.waits.Load()).To(Equal(int32(4)))
		Expect(wrongAuthentication.Load()).To(BeFalse())
	})

	It("keeps file credentials and TLS stable until reopened while observing fresh identity only", func() {
		var reads atomic.Int32
		var uid atomic.Value
		uid.Store("cluster-one")
		requests := make(chan string, 16)
		tokens := make(chan string, 16)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reads.Add(1)
			requests <- r.URL.Path
			tokens <- r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/prefix/api/v1/namespaces/kube-system":
				fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":%q}}`, uid.Load())
			case "/prefix/version":
				fmt.Fprint(w, `{"gitVersion":"v1.35.0"}`)
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		}))
		defer server.Close()
		dir := GinkgoT().TempDir()
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		caFile, tokenFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
		Expect(os.WriteFile(caFile, ca, 0600)).To(Succeed())
		Expect(os.WriteFile(tokenFile, []byte("original-token\n"), 0600)).To(Succeed())
		filename := writeKubeconfig(dir, &clientapi.Cluster{Server: server.URL + "/prefix", CertificateAuthority: "ca.pem", TLSServerName: "example.com"}, &clientapi.AuthInfo{TokenFile: "token"})
		request := config.Request{Kubeconfig: filename, RequestTimeout: time.Second}
		access, err := managementaccess.Open(request, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(reads.Load()).To(BeZero())
		Expect(access.Namespace).To(Equal("context-namespace"))
		Expect(access.REST.Timeout).To(Equal(time.Second))
		Expect(access.REST.CAFile).To(BeEmpty())
		Expect(access.REST.BearerTokenFile).To(BeEmpty())
		Expect(os.Remove(caFile)).To(Succeed())
		Expect(os.WriteFile(tokenFile, []byte("replacement-token"), 0600)).To(Succeed())
		expectedTrust := inventory.Digest("management-trust-v1", []string{fmt.Sprintf("sha256:%x", sha256.Sum256(ca)), "example.com"})
		for _, expectedUID := range []string{"cluster-one", "cluster-two"} {
			uid.Store(expectedUID)
			identity, err := access.ObserveIdentity(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.SystemNamespaceUID).To(Equal(expectedUID))
			Expect(identity.APIEndpoint).To(Equal(server.URL + "/prefix"))
			Expect(identity.KubernetesVersion).To(Equal("v1.35.0"))
			Expect(identity.TrustDigest).To(Equal(expectedTrust))
			Expect(identity.EvidenceRefs).To(Equal([]string{"management-identity"}))
			Expect(identity.Nodes).To(BeNil())
			Expect(identity.Observations).To(BeNil())
			Expect(<-requests).To(Equal("/prefix/api/v1/namespaces/kube-system"))
			Expect(<-requests).To(Equal("/prefix/version"))
			Expect(<-tokens).To(Equal("Bearer original-token"))
			Expect(<-tokens).To(Equal("Bearer original-token"))
		}
		Expect(reads.Load()).To(Equal(int32(4)))
		_, err = managementaccess.Open(request, nil)
		Expect(err).To(MatchError("invalid bounded management auth/trust reference"))
		Expect(os.WriteFile(caFile, ca, 0600)).To(Succeed())
		request.Namespace = "explicit-namespace"
		reopened, err := managementaccess.Open(request, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(reopened.Namespace).To(Equal("explicit-namespace"))
		_, err = reopened.ObserveIdentity(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(<-tokens).To(Equal("Bearer replacement-token"))
		Expect(<-tokens).To(Equal("Bearer replacement-token"))
	})

	It("rejects unsupported authentication, ambiguous credentials and unverified endpoints", func() {
		cases := []struct {
			name    string
			cluster clientapi.Cluster
			auth    clientapi.AuthInfo
		}{
			{name: "anonymous", cluster: clientapi.Cluster{Server: "https://example.invalid"}},
			{name: "exec", auth: clientapi.AuthInfo{Exec: &clientapi.ExecConfig{Command: "must-never-execute"}}},
			{name: "provider", auth: clientapi.AuthInfo{AuthProvider: &clientapi.AuthProviderConfig{Name: "must-never-load"}}},
			{name: "basic", auth: clientapi.AuthInfo{Username: "credential-canary", Password: "credential-canary"}},
			{name: "ambiguous-token", auth: clientapi.AuthInfo{Token: "credential-canary", TokenFile: "must-not-read"}},
			{name: "ambiguous-ca", cluster: clientapi.Cluster{CertificateAuthority: "must-not-read", CertificateAuthorityData: []byte("credential-canary")}},
			{name: "ambiguous-certificate", auth: clientapi.AuthInfo{ClientCertificate: "must-not-read", ClientCertificateData: []byte("credential-canary")}},
			{name: "ambiguous-key", auth: clientapi.AuthInfo{ClientKey: "must-not-read", ClientKeyData: []byte("credential-canary")}},
			{name: "http", cluster: clientapi.Cluster{Server: "http://example.invalid"}},
			{name: "userinfo", cluster: clientapi.Cluster{Server: "https://credential-canary@example.invalid"}},
			{name: "query", cluster: clientapi.Cluster{Server: "https://example.invalid?credential-canary"}},
			{name: "fragment", cluster: clientapi.Cluster{Server: "https://example.invalid#credential-canary"}},
			{name: "insecure", cluster: clientapi.Cluster{InsecureSkipTLSVerify: true}},
		}
		for _, test := range cases {
			By(test.name)
			if test.cluster.Server == "" {
				test.cluster.Server = "https://example.invalid"
			}
			if test.name != "anonymous" && test.auth.Token == "" && test.auth.TokenFile == "" {
				test.auth.Token = "credential-canary"
			}
			filename := writeKubeconfig(GinkgoT().TempDir(), &test.cluster, &test.auth)
			_, err := managementaccess.Open(config.Request{Kubeconfig: filename}, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("credential-canary"))
			Expect(err.Error()).NotTo(ContainSubstring("must-not-read"))
		}
	})

	It("requires an explicit bounded kubeconfig even when ambient access exists", func() {
		filename := writeKubeconfig(GinkgoT().TempDir(), &clientapi.Cluster{Server: "https://example.invalid"}, &clientapi.AuthInfo{Token: "credential-canary"})
		GinkgoT().Setenv("KUBECONFIG", filename)
		_, err := managementaccess.Open(config.Request{}, nil)
		Expect(err).To(MatchError("invalid bounded management kubeconfig"))
		oversized := filepath.Join(GinkgoT().TempDir(), "large")
		Expect(os.WriteFile(oversized, make([]byte, (16<<20)+1), 0600)).To(Succeed())
		_, err = managementaccess.Open(config.Request{Kubeconfig: oversized}, nil)
		Expect(err).To(MatchError("invalid bounded management kubeconfig"))
	})

	It("rejects oversized token and CA references without exposing their contents or paths", func() {
		for _, kind := range []string{"token", "ca"} {
			dir := GinkgoT().TempDir()
			reference := filepath.Join(dir, "credential-canary")
			cluster := &clientapi.Cluster{Server: "https://example.invalid"}
			auth := &clientapi.AuthInfo{Token: "credential-canary"}
			limit := 1 << 20
			expected := "invalid bounded management auth/trust reference"
			if kind == "token" {
				auth.Token, auth.TokenFile = "", reference
				limit, expected = 64<<10, "invalid bounded token reference"
			} else {
				cluster.CertificateAuthority = reference
			}
			Expect(os.WriteFile(reference, make([]byte, limit+1), 0600)).To(Succeed())
			filename := writeKubeconfig(dir, cluster, auth)
			_, err := managementaccess.Open(config.Request{Kubeconfig: filename}, nil)
			Expect(err).To(MatchError(expected))
		}
	})
})

var _ = Describe("fresh management identity reads", func() {
	It("honors cancellation during either remote read and returns no partial identity", func() {
		for _, blocked := range []string{"/api/v1/namespaces/kube-system", "/version"} {
			started := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == blocked {
					close(started)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"uid":"cluster-one"}}`)
			}))
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			filename := writeKubeconfig(GinkgoT().TempDir(), &clientapi.Cluster{Server: server.URL, CertificateAuthorityData: ca}, &clientapi.AuthInfo{Token: "credential-canary"})
			access, err := managementaccess.Open(config.Request{Kubeconfig: filename, RequestTimeout: time.Second}, nil)
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				identity, err := access.ObserveIdentity(ctx)
				if identity != nil {
					done <- fmt.Errorf("unexpected partial identity")
					return
				}
				done <- err
			}()
			Eventually(started, time.Second).Should(BeClosed())
			cancel()
			Eventually(done, time.Second).Should(Receive(MatchError(context.Canceled)))
			before := calls.Load()
			identity, err := access.ObserveIdentity(ctx)
			Expect(identity).To(BeNil())
			Expect(err).To(MatchError(context.Canceled))
			Expect(calls.Load()).To(Equal(before))
			server.Close()
		}
	})

	It("returns redacted identity/version errors without retaining partial evidence", func() {
		for _, response := range []string{"identity-error", "empty-uid", "version-error", "invalid-version", "empty-version"} {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					if response == "identity-error" {
						http.Error(w, "credential-canary", http.StatusForbidden)
						return
					}
					uid := "cluster-one"
					if response == "empty-uid" {
						uid = ""
					}
					fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"uid":%q}}`, uid)
					return
				}
				switch response {
				case "version-error":
					http.Error(w, "credential-canary", http.StatusForbidden)
				case "invalid-version":
					fmt.Fprint(w, "credential-canary")
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			filename := writeKubeconfig(GinkgoT().TempDir(), &clientapi.Cluster{Server: server.URL, CertificateAuthorityData: ca}, &clientapi.AuthInfo{Token: "credential-canary"})
			access, err := managementaccess.Open(config.Request{Kubeconfig: filename, RequestTimeout: time.Second}, nil)
			Expect(err).NotTo(HaveOccurred())
			identity, err := access.ObserveIdentity(context.Background())
			Expect(identity).To(BeNil())
			expected := "management version read failed"
			if response == "identity-error" {
				expected = "management cluster identity read failed"
			}
			if response == "empty-uid" {
				expected = "management identity UID is empty"
			}
			Expect(err).To(MatchError(expected))
			server.Close()
		}
	})
})

// Counting is local to this constructor test; production never installs a
// process-global limiter or a preauthenticated transport.
type constructorRateLimiter struct {
	flowcontrol.RateLimiter
	waits atomic.Int32
}

func (r *constructorRateLimiter) Wait(ctx context.Context) error {
	r.waits.Add(1)
	return r.RateLimiter.Wait(ctx)
}
