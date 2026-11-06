// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

var _ = Describe("explicit management access", func() {
	It("freezes CA files and preserves a proxy base path while observing three nodes", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer test-token"))
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/prefix/version":
				json.NewEncoder(w).Encode(map[string]any{"gitVersion": "v1.34.0"})
			case "/prefix/api/v1/namespaces/kube-system":
				w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"cluster-one"}}`))
			case "/prefix/api/v1/nodes":
				w.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[{"metadata":{"name":"cp1","uid":"node1","labels":{"node-role.kubernetes.io/control-plane":""}},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux"},"conditions":[{"type":"Ready","status":"True"}]}}]}`))
			default:
				w.WriteHeader(404)
			}
		}))
		defer server.Close()
		dir, e := os.MkdirTemp("", "dpf-management-")
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		caPath := filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(caPath, ca, 0600)).To(Succeed())
		raw := clientapi.Config{CurrentContext: "management", Clusters: map[string]*clientapi.Cluster{"management": {Server: server.URL + "/prefix", CertificateAuthority: caPath}}, AuthInfos: map[string]*clientapi.AuthInfo{"reader": {Token: "test-token"}}, Contexts: map[string]*clientapi.Context{"management": {Cluster: "management", AuthInfo: "reader"}}}
		b, e := clientcmd.Write(raw)
		Expect(e).NotTo(HaveOccurred())
		filename := filepath.Join(dir, "config")
		Expect(os.WriteFile(filename, b, 0600)).To(Succeed())
		mg, e := ManagementAccess(config.Request{Kubeconfig: filename, RequestTimeout: time.Second})
		Expect(e).NotTo(HaveOccurred())
		Expect(mg.REST.CAData).To(Equal(ca))
		Expect(mg.REST.CAFile).To(BeEmpty())
		observed, e := mg.Observe(context.Background())
		Expect(e).NotTo(HaveOccurred())
		Expect(observed.SystemNamespaceUID).To(Equal("cluster-one"))
		Expect(observed.TrustDigest).NotTo(Equal("system-roots"))
		Expect(observed.Nodes).To(HaveLen(1))
	})
	It("rejects anonymous, exec and HTTP configurations before client construction", func() {
		dir, e := os.MkdirTemp("", "dpf-management-")
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		for _, test := range []struct {
			server string
			auth   *clientapi.AuthInfo
		}{{"https://example.invalid", &clientapi.AuthInfo{}}, {"http://example.invalid", &clientapi.AuthInfo{Token: "private-token"}}, {"https://example.invalid", &clientapi.AuthInfo{Exec: &clientapi.ExecConfig{Command: "must-never-execute"}}}} {
			raw := clientapi.Config{CurrentContext: "m", Clusters: map[string]*clientapi.Cluster{"m": {Server: test.server}}, AuthInfos: map[string]*clientapi.AuthInfo{"r": test.auth}, Contexts: map[string]*clientapi.Context{"m": {Cluster: "m", AuthInfo: "r"}}}
			b, e := clientcmd.Write(raw)
			Expect(e).NotTo(HaveOccurred())
			filename := filepath.Join(dir, "config")
			Expect(os.WriteFile(filename, b, 0600)).To(Succeed())
			_, e = ManagementAccess(config.Request{Kubeconfig: filename})
			Expect(e).To(HaveOccurred())
			Expect(e.Error()).NotTo(ContainSubstring("private-token"))
		}
	})
})

var _ = Describe("management observation failures", func() {
	It("distinguishes unavailable initial/recheck observations from changed identities", func() {
		for _, initialFailure := range []bool{true, false} {
			var reads atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces/kube-system":
					if reads.Add(1) > 1 || initialFailure {
						w.WriteHeader(403)
						return
					}
					w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"cluster-one"}}`))
				case "/version":
					w.Write([]byte(`{"gitVersion":"v1.34.0"}`))
				case "/api/v1/nodes":
					w.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","items":[]}`))
				default:
					w.WriteHeader(404)
				}
			}))
			raw := clientapi.Config{CurrentContext: "m", Clusters: map[string]*clientapi.Cluster{"m": {Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, AuthInfos: map[string]*clientapi.AuthInfo{"r": {Token: "test-token"}}, Contexts: map[string]*clientapi.Context{"m": {Cluster: "m", AuthInfo: "r"}}}
			data, err := clientcmd.Write(raw)
			Expect(err).NotTo(HaveOccurred())
			filename := filepath.Join(GinkgoT().TempDir(), "management.kubeconfig")
			Expect(os.WriteFile(filename, data, 0600)).To(Succeed())
			input := config.Input{}
			if initialFailure {
				input.Site.Inventory = &inventory.Inventory{ScopeDigest: "known-baseline"}
			}
			report := Collect(context.Background(), config.Request{Input: input, Kubeconfig: filename, Timeout: 10 * time.Second, RequestTimeout: time.Second, MaxConcurrentBMCs: 1, Mode: "inventory"}, "test", Dependencies{})
			server.Close()
			codes := []string{}
			for _, problem := range report.Errors {
				codes = append(codes, problem.Code)
			}
			if initialFailure {
				Expect(codes).To(ContainElement("MANAGEMENT_INSPECTION_FAILED"))
			} else {
				Expect(codes).To(ContainElement("MANAGEMENT_RECHECK_FAILED"))
			}
			Expect(codes).NotTo(ContainElement("SCOPE_CONFLICT"))
			Expect(codes).NotTo(ContainElement("MANAGEMENT_SCOPE_CHANGED"))
			Expect(report.Discovery.Outcome).To(Equal("partial"))
			Expect(ExitCode(report)).To(Equal(3))
		}
	})
})
