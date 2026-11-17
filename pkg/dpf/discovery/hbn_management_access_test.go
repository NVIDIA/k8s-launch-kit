// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

var _ = Describe("HBN management collection integration", func() {
	It("uses explicit namespace and access, retains denied reads and rechecks cluster identity", func() {
		var mu sync.Mutex
		var methods, paths []string
		identityReads := 0
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			methods = append(methods, r.Method)
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") != "Bearer explicit-test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/api/v1/namespaces/kube-system":
				identityReads++
				uid := "original-cluster"
				if identityReads > 1 {
					uid = "replacement-cluster"
				}
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"` + uid + `"}}`))
			case "/version":
				_, _ = w.Write([]byte(`{"gitVersion":"v1.35.4"}`))
			case "/api/v1/nodes":
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"NodeList","items":[]}`))
			case "/apis/apps/v1/namespaces/selected-dpf/deployments":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403}`))
			case "/apis/storage.k8s.io/v1/storageclasses":
				_, _ = w.Write([]byte(`{"apiVersion":"storage.k8s.io/v1","kind":"StorageClassList","items":[]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`))
			}
		}))
		defer server.Close()
		raw := clientapi.Config{CurrentContext: "selected", Clusters: map[string]*clientapi.Cluster{"selected": {Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, AuthInfos: map[string]*clientapi.AuthInfo{"reader": {Token: "explicit-test-token"}}, Contexts: map[string]*clientapi.Context{"selected": {Cluster: "selected", AuthInfo: "reader", Namespace: "context-namespace-must-not-be-used"}}}
		data, err := clientcmd.Write(raw)
		Expect(err).NotTo(HaveOccurred())
		filename := filepath.Join(GinkgoT().TempDir(), "management.kubeconfig")
		Expect(os.WriteFile(filename, data, 0600)).To(Succeed())
		req := inventoryRequest()
		req.Kubeconfig, req.DPFNamespace = filename, "selected-dpf"
		req.Timeout, req.RequestTimeout = 10*time.Second, time.Second
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		req.HBN = &profile
		release, err := releases.Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		req.Software = &release
		report := Collect(context.Background(), req, "test-run", Dependencies{BMC: fixtureCollector{admin: true}})
		Expect(report.Readiness).NotTo(BeNil())
		Expect(report.Readiness.State).To(Equal("blocked"))
		Expect(report.Inventory.HBN.Namespace).To(Equal("selected-dpf"))
		Expect(report.Inventory.HBN.Management).NotTo(BeNil())
		codes := []string{}
		for _, problem := range report.Errors {
			codes = append(codes, problem.Code)
		}
		Expect(codes).To(ContainElement("MANAGEMENT_SCOPE_CHANGED"))
		Expect(ExitCode(report)).To(Equal(3))
		mu.Lock()
		defer mu.Unlock()
		Expect(paths).To(ContainElement("/apis/apps/v1/namespaces/selected-dpf/deployments"))
		Expect(identityReads).To(Equal(2))
		for _, method := range methods {
			Expect(method).To(Equal(http.MethodGet))
		}
	})
})
