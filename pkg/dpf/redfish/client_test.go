// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedfish(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF Redfish") }

var _ = Describe("GET/session-only transport", func() {
	It("logs in through public root session links without reading the protected service", func() {
		for _, scenario := range []struct{ slash, absolute bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			collection := "/redfish/v1/SessionService/Sessions"
			receipt := collection + "/owned"
			if scenario.slash {
				collection += "/"
				receipt += "/"
			}
			var methods []string
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				prefix := ""
				if scenario.absolute {
					prefix = "https://" + r.Host
				}
				switch r.URL.Path {
				case "/redfish/v1/":
					json.NewEncoder(w).Encode(Document{"SessionService": Document{"@odata.id": "/redfish/v1/SessionService"}, "Links": Document{"Sessions": Document{"@odata.id": prefix + collection}}})
				case collection:
					Expect(r.Method).To(Equal("POST"))
					w.Header().Set("Location", prefix+receipt)
					w.Header().Set("X-Auth-Token", "private-token")
					w.WriteHeader(http.StatusCreated)
					w.Write([]byte(`{}`))
				case receipt:
					Expect(r.Method).To(Equal("DELETE"))
					Expect(r.Header.Get("X-Auth-Token")).To(Equal("private-token"))
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusUnauthorized)
				}
			}))
			c := &Client{origin: s.URL, http: s.Client()}
			_, err := c.Open(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Close(context.Background())).To(Succeed())
			Expect(methods).To(Equal([]string{"GET /redfish/v1/", "POST " + collection, "DELETE " + receipt}))
			s.Close()
		}
	})
	It("rejects foreign, OEM and query-bearing session links without attempting login", func() {
		for _, link := range []string{"https://foreign.invalid/redfish/v1/SessionService/Sessions", "/redfish/v1/Oem/Sessions", "/redfish/v1/SessionService/Sessions?$skip=1"} {
			var writes atomic.Int32
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				json.NewEncoder(w).Encode(Document{"Links": Document{"Sessions": Document{"@odata.id": link}}})
			}))
			c := &Client{origin: s.URL, http: s.Client()}
			_, err := c.Open(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(writes.Load()).To(BeZero())
			Expect(c.Close(context.Background())).To(Succeed())
			s.Close()
		}
	})
	It("does not accept the session collection itself as a cleanup receipt", func() {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/")
				w.Header().Set("X-Auth-Token", "private-token")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{}`))
				return
			}
			Expect(r.Method).NotTo(Equal(http.MethodDelete))
			json.NewEncoder(w).Encode(Document{"Links": Document{"Sessions": Document{"@odata.id": "/redfish/v1/SessionService/Sessions/"}}})
		}))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		_, err := c.Open(context.Background())
		Expect(Code(err)).To(Equal("INVALID_SESSION_RECEIPT"))
		Expect(Code(c.Close(context.Background()))).To(Equal("SESSION_OUTCOME_UNKNOWN"))
	})
	It("requires explicit insecure opt-in for an untrusted server and still rejects foreign origins", func() {
		var requests atomic.Int32
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Write([]byte(`{"RedfishVersion":"1.0.0"}`))
		}))
		defer s.Close()
		Expect(os.Setenv("DPF_TLS_TEST_PASSWORD", "private-password")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TLS_TEST_PASSWORD")
		for _, policy := range []string{"", ", tls: {insecureSkipVerify: false}", ", tls: {insecureSkipVerify: true}"} {
			source := fmt.Sprintf("management:\n  kubeconfigRef: {file: unused}\ncredentials:\n  read: {username: reader, passwordRef: {env: DPF_TLS_TEST_PASSWORD}}\ndiscovery:\n  dpus: [{bmcEndpoint: '%s', credentialRef: read%s}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]\n", s.URL, policy)
			in, err := config.Decode([]byte(source), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			req, err := config.Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			c := New(req.Targets[0], time.Second)
			_, err = c.Get(context.Background(), "/redfish/v1/")
			if req.Targets[0].Access.InsecureSkipVerify() {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(Code(err)).To(Equal("TLS_VERIFICATION_FAILED"))
				Expect(requests.Load()).To(BeZero())
			}
			_, err = c.Get(context.Background(), "https://foreign.invalid/redfish/v1/")
			Expect(Code(err)).To(Equal("CROSS_ORIGIN_LINK"))
		}
		Expect(requests.Load()).To(Equal(int32(1)))
	})
	It("allows the service root and logs out exactly its own session", func() {
		var methods []string
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/redfish/v1/":
				json.NewEncoder(w).Encode(Document{"SessionService": Document{"@odata.id": "/redfish/v1/SessionService"}})
			case "/redfish/v1/SessionService":
				json.NewEncoder(w).Encode(Document{"Sessions": Document{"@odata.id": "/redfish/v1/SessionService/Sessions"}})
			case "/redfish/v1/SessionService/Sessions":
				Expect(r.Method).To(Equal("POST"))
				w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/own")
				w.Header().Set("X-Auth-Token", "private-token")
				w.WriteHeader(201)
				w.Write([]byte(`{}`))
			case "/redfish/v1/SessionService/Sessions/own":
				Expect(r.Method).To(Equal("DELETE"))
				Expect(r.Header.Get("X-Auth-Token")).To(Equal("private-token"))
				w.WriteHeader(204)
			default:
				w.WriteHeader(404)
			}
		}))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		_, e := c.Open(context.Background())
		Expect(e).NotTo(HaveOccurred())
		Expect(c.Close(context.Background())).To(Succeed())
		Expect(methods).To(Equal([]string{"GET /redfish/v1/", "GET /redfish/v1/SessionService", "POST /redfish/v1/SessionService/Sessions", "DELETE /redfish/v1/SessionService/Sessions/own"}))
	})
	It("rejects unsafe links before credentials leave the process", func() {
		c := &Client{origin: "https://example.invalid"}
		for _, u := range []string{"https://elsewhere.invalid/redfish/v1/Systems", "http://example.invalid/redfish/v1/Systems", "/redfish/v1/%2e%2e/Accounts", "/redfish/v1/../Accounts", "/redfish/v1/Systems?secret=1", "//elsewhere.invalid/redfish/v1/Systems"} {
			_, e := c.guard(u)
			Expect(e).To(HaveOccurred())
		}
	})
	It("follows only bounded same-collection pages and checks count", func() {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/redfish/v1/Systems":
				if r.URL.RawQuery == "" {
					json.NewEncoder(w).Encode(Document{"Members@odata.count": 2, "Members": []any{Document{"@odata.id": "/redfish/v1/Systems/a"}}, "Members@odata.nextLink": "/redfish/v1/Systems?$skip=1"})
				} else {
					json.NewEncoder(w).Encode(Document{"Members@odata.count": 2, "Members": []any{Document{"@odata.id": "/redfish/v1/Systems/b"}}})
				}
			default:
				w.Write([]byte(`{"UUID":"example"}`))
			}
		}))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		members, e := c.Collection(context.Background(), "/redfish/v1/Systems")
		Expect(e).NotTo(HaveOccurred())
		Expect(members).To(HaveLen(2))
	})
	It("never retries authorization failures or prints response secrets", func() {
		calls := 0
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(401)
			w.Write([]byte("private-password"))
		}))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		_, e := c.Get(context.Background(), "/redfish/v1/Systems")
		Expect(e).To(HaveOccurred())
		Expect(e.Error()).NotTo(ContainSubstring("private-password"))
		Expect(calls).To(Equal(1))
	})
	It("stops read retries at cancellation", func() {
		ctx, cancel := context.WithCancel(context.Background())
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); w.WriteHeader(503) }))
		defer s.Close()
		c := &Client{origin: s.URL, http: s.Client()}
		start := time.Now()
		_, e := c.Get(ctx, "/redfish/v1/Systems")
		Expect(e).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})
	It("rejects placeholder physical serials", func() {
		for _, v := range []string{"", "unknown", "N/A", "00000", "to be filled by o.e.m."} {
			Expect(ValidSerial(v)).To(BeFalse())
		}
		Expect(ValidSerial(" 000ABC-01 ")).To(BeTrue())
	})
})
