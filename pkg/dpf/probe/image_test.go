//go:build dpf_probe_image

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package probe

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This explicit integration gate runs existing image tools without a cluster.
// DPF_PROBE_TEST_IMAGE must be the immutable catalog image under review.
var _ = Describe("published image runtime", func() {
	It("collects both vantages with real DNS, per-IP TLS, HTTP and read-only routes", func() {
		image := os.Getenv("DPF_PROBE_TEST_IMAGE")
		Expect(image).To(ContainSubstring("@sha256:"))
		platform := os.Getenv("DPF_PROBE_TEST_PLATFORM")
		if platform == "" {
			platform = "linux/amd64"
		}
		Expect(platform).To(BeElementOf("linux/amd64", "linux/arm64"))
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/401":
				w.WriteHeader(401)
			case "/403":
				w.WriteHeader(403)
			case "/302":
				w.Header().Set("Location", "https://must-not-follow.invalid")
				w.WriteHeader(302)
			case "/404":
				w.WriteHeader(404)
			case "/500":
				w.WriteHeader(500)
			case "/invalid":
				fmt.Fprint(w, "not JSON")
			case "/large":
				fmt.Fprint(w, `{"value":"`)
				fmt.Fprint(w, string(make([]byte, 70000)))
				fmt.Fprint(w, `"}`)
			case "/multi":
				fmt.Fprint(w, "{}\n{}")
			case "/scalar-object":
				fmt.Fprint(w, "42\n{}")
			default:
				fmt.Fprint(w, `{"ok":true}`)
			}
		}))
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		Expect(err).NotTo(HaveOccurred())
		server.Listener.Close()
		server.Listener = listener
		server.StartTLS()
		defer server.Close()
		port := listener.Addr().(*net.TCPAddr).Port
		endpoint := fmt.Sprintf("https://host.docker.internal:%d", port)
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		req := Request{Version: ProtocolVersion, RunID: "local-image-check", Deadline: time.Now().Add(2 * time.Minute), RequestTimeout: 3 * time.Second, Destinations: []Destination{}}
		for _, p := range []string{"/ok", "/401", "/403", "/302", "/404", "/500", "/invalid", "/large", "/multi", "/scalar-object"} {
			req.Destinations = append(req.Destinations, Destination{ID: p, Endpoint: endpoint, Path: p, ServerName: "example.com", CA: ca})
		}
		req.Destinations = append(req.Destinations, Destination{ID: "untrusted", Endpoint: endpoint, Path: "/ok", ServerName: "example.com"}, Destination{ID: "dns-missing", Endpoint: "https://does-not-exist.invalid", Path: "/version"})
		for _, mode := range []string{"pod-network", "node-network"} {
			dir := GinkgoT().TempDir()
			Expect(os.Chmod(dir, 0755)).To(Succeed())
			req.Deadline = time.Now().Add(2 * time.Minute)
			files, err := requestFiles(req)
			Expect(err).NotTo(HaveOccurred())
			for key, value := range files {
				Expect(os.WriteFile(filepath.Join(dir, key), []byte(value), 0644)).To(Succeed())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			command := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", platform, "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "64m", "--env", "POD_UID=pod-local", "--mount", "type=bind,src="+dir+",dst=/request,readonly", "--tmpfs", "/scratch:rw,size=16777216,uid=65532,gid=65532,mode=0700", "--workdir", "/scratch", "--entrypoint", "/bin/bash", image, "/request/collect.sh", "/request/request.json", "node-local", mode)
			output, err := command.CombinedOutput()
			cancel()
			GinkgoWriter.Printf("image %s %s output: %s\n", image, mode, output)
			Expect(err).NotTo(HaveOccurred(), string(output))
			var result Response
			Expect(json.Unmarshal(output, &result)).To(Succeed())
			Expect(ValidateResponse(result, req, "node-local", "pod-local", mode)).To(Succeed())
			expected := []string{"observed", "authentication-required", "authentication-required", "redirect-refused", "resource-unavailable", "application-error", "invalid-response", "invalid-response", "invalid-response", "invalid-response"}
			for i, state := range expected {
				Expect(result.Results[i].Addresses).NotTo(BeEmpty())
				verified := 0
				for _, a := range result.Results[i].Addresses {
					if a.TLS == "verified" {
						verified++
						Expect(a.TCP).To(Equal("reachable"))
						Expect(a.HTTP).To(Equal(state))
					} else {
						// Docker Desktop can advertise an unroutable IPv6 host
						// alternative. Retain its failure; do not drop that IP.
						Expect(a.HTTP).To(Equal("http-unavailable"))
					}
				}
				Expect(verified).To(BeNumerically(">", 0))
			}
			Expect(result.Results[10].Addresses).NotTo(BeEmpty())
			reachable := false
			for _, a := range result.Results[10].Addresses {
				reachable = reachable || a.TCP == "reachable"
				Expect(a.TLS).To(Equal("unavailable"))
			}
			Expect(reachable).To(BeTrue())
			Expect(result.Results[11].DNS).To(Equal("unavailable"))
			if mode == "node-network" {
				Expect(result.Interfaces).NotTo(BeEmpty())
				Expect(result.Routes).NotTo(BeEmpty())
			} else {
				Expect(result.Interfaces).To(BeEmpty())
				Expect(result.Routes).To(BeEmpty())
			}
		}
	})
})
