// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("observation and cleanup diagnostics", func() {
	It("retains independent forbidden read and DELETE metadata with the exact recovery receipt", func() {
		var writes []string
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writes = append(writes, r.Method+" "+r.URL.Path)
			}
			switch r.URL.Path {
			case "/redfish/v1/":
				json.NewEncoder(w).Encode(Document{"Links": Document{"Sessions": Document{"@odata.id": "/redfish/v1/SessionService/Sessions"}}, "Systems": Document{"@odata.id": "/redfish/v1/Systems"}})
			case "/redfish/v1/SessionService/Sessions":
				w.Header().Set("X-Auth-Token", "secret-token")
				w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/owned")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{}`))
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		}))
		defer s.Close()
		Expect(os.Setenv("DPF_DIAGNOSTIC_PASSWORD", "secret-password")).To(Succeed())
		defer os.Unsetenv("DPF_DIAGNOSTIC_PASSWORD")
		source := fmt.Sprintf("management:\n  kubeconfigRef: {file: unused}\ncredentials:\n  read: {username: reader, passwordRef: {env: DPF_DIAGNOSTIC_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  dpus: [{bmcEndpoint: '%s', credentialRef: read}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n", s.URL)
		in, err := config.Decode([]byte(source), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		req, err := config.Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		device, _, err := (Collector{}).Collect(context.Background(), req.Targets[0], time.Second)
		Expect(err).To(HaveOccurred())
		Expect(device.Authentication.PasswordRead).NotTo(Equal("verified"))
		Expect(device.ReasonCode).To(Equal("CLEANUP_INCOMPLETE"))
		Expect(Code(err)).To(Equal("ACCESS_DENIED"))
		Expect(Diagnostics(err)).To(Equal([]Diagnostic{
			{Code: "ACCESS_DENIED", Method: "GET", URI: "/redfish/v1/Systems", Status: 403, Stage: "observation"},
			{Code: "ACCESS_DENIED", Method: "DELETE", URI: "/redfish/v1/SessionService/Sessions/owned", Status: 403, Stage: "cleanup"},
		}))
		Expect(CleanupReceipt(err)).To(Equal("/redfish/v1/SessionService/Sessions/owned"))
		Expect(err.Error()).NotTo(ContainSubstring("secret-"))
		Expect(writes).To(Equal([]string{"POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/owned"}))
	})
	It("omits query tokens and response bodies from diagnostics", func() {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"secret":"private-body"}`))
		}))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		_, err := c.Get(context.Background(), "/redfish/v1/Systems?$skiptoken=private-query")
		Expect(Diagnostics(err)).To(Equal([]Diagnostic{{Code: "ACCESS_DENIED", Method: "GET", URI: "/redfish/v1/Systems", Status: 403, Stage: "observation"}}))
		Expect(strings.Contains(err.Error(), "private")).To(BeFalse())
		Expect(CleanupReceipt(err)).To(BeEmpty())
		Expect(Diagnostics(nil)).To(BeEmpty())
	})
})
