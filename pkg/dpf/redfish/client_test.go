// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"encoding/json"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRedfish(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF Redfish") }

var _ = Describe("GET/session-only transport", func() {
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
