// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/zapr"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type progressBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *progressBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *progressBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

var _ = Describe("secret-safe Redfish progress and logs", func() {
	It("selects debug/trace metadata without logging URLs, bodies, tokens or errors", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"private":"response-body-canary"}`))
		}))
		DeferCleanup(server.Close)
		for _, level := range []zapcore.Level{zapcore.InfoLevel, zapcore.DebugLevel, -2} {
			core, records := observer.New(level)
			ctx := log.IntoContext(context.Background(), zapr.NewLogger(zap.New(core)))
			c := &Client{origin: server.URL, http: server.Client(), token: "token-canary", session: "/redfish/v1/SessionService/Sessions/session-id-canary"}
			_, err := c.Get(ctx, "/redfish/v1/AccountService/Accounts/username-canary?$skiptoken=query-canary")
			Expect(Code(err)).To(Equal("ACCESS_DENIED"))
			_, err = c.Get(ctx, "https://userinfo-canary:password-canary@foreign.invalid/redfish/v1/Systems")
			Expect(err).To(HaveOccurred())
			Expect(Code(c.Close(ctx))).To(Equal("ACCESS_DENIED"))
			if level == zapcore.InfoLevel {
				Expect(records.Len()).To(BeZero())
			} else {
				Expect(records.FilterMessage("Redfish session cleanup completed").Len()).To(Equal(1))
			}
			if level == -2 {
				requests := records.FilterMessage("Redfish request completed").All()
				Expect(requests).To(HaveLen(3))
				Expect(requests[0].ContextMap()).To(HaveKeyWithValue("httpStatus", int64(403)))
				Expect(requests[0].ContextMap()).To(HaveKey("duration"))
			} else {
				Expect(records.FilterMessage("Redfish request completed").Len()).To(BeZero())
			}
			encoded, err := json.Marshal(records.All())
			Expect(err).NotTo(HaveOccurred())
			for _, secret := range []string{"response-body-canary", "token-canary", "session-id-canary", "username-canary", "query-canary", "userinfo-canary", "password-canary"} {
				Expect(string(encoded)).NotTo(ContainSubstring(secret))
			}
		}
	})

	It("emits live stages without host evidence on success or deferred cleanup failure", func() {
		for _, cleanupFails := range []bool{false, true} {
			docs := hostGraph()
			docs["/redfish/v1/Systems/host"]["Manufacturer"] = "HPE"
			docs["/redfish/v1/Systems/host"]["Model"] = "Host"
			docs["/redfish/v1/Systems/host"]["UUID"] = "11111111-1111-4111-8111-111111111111"
			link := func(uri string) Document { return Document{"@odata.id": uri} }
			if !cleanupFails {
				docs["/redfish/v1/Managers"] = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Managers/BMC")}}
				docs["/redfish/v1/Managers/BMC"] = Document{"FirmwareVersion": "1.0", "DateTime": "2026-11-13T12:00:00Z", "NetworkProtocol": link("/redfish/v1/Managers/BMC/NetworkProtocol")}
				docs["/redfish/v1/Managers/BMC/NetworkProtocol"] = Document{"NTP": Document{"ProtocolEnabled": true, "NTPServers": []any{"ntp.example.invalid"}}}
				docs["/redfish/v1/UpdateService"] = Document{"FirmwareInventory": link("/redfish/v1/UpdateService/FirmwareInventory")}
				members := []any{}
				for _, component := range []string{"bmc", "cec", "atf", "uefi", "nic", "dpu-os", "dpu-bsp", "golden"} {
					uri := "/redfish/v1/UpdateService/FirmwareInventory/" + component
					members = append(members, link(uri))
					docs[uri] = Document{"Id": component, "Version": "1.0"}
				}
				docs["/redfish/v1/UpdateService/FirmwareInventory"] = Document{"Members@odata.count": len(members), "Members": members}
			}
			entered, rechecking, cleaning := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseRead, releaseRecheck, releaseCleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := func(ch chan struct{}) {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
			var systemReads atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uri := strings.TrimSuffix(r.URL.Path, "/")
				var doc Document
				switch uri {
				case "/redfish/v1":
					doc = Document{"Links": Document{"Sessions": link("/redfish/v1/SessionService/Sessions")}, "Systems": link("/redfish/v1/Systems"), "private": "response-body-canary"}
					if !cleanupFails {
						doc["Managers"] = link("/redfish/v1/Managers")
						doc["UpdateService"] = link("/redfish/v1/UpdateService")
					}
				case "/redfish/v1/SessionService/Sessions":
					w.Header().Set("X-Auth-Token", "token-canary")
					w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/session-id-canary")
					w.WriteHeader(http.StatusCreated)
					doc = Document{}
				case "/redfish/v1/SessionService/Sessions/session-id-canary":
					close(cleaning)
					<-releaseCleanup
					if cleanupFails {
						w.WriteHeader(http.StatusForbidden)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
					return
				case "/redfish/v1/Systems":
					close(entered)
					<-releaseRead
					doc = Document{"Members@odata.count": 1, "Members": []any{link("/redfish/v1/Systems/host")}}
				default:
					var ok bool
					doc, ok = docs[uri]
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if uri == "/redfish/v1/Systems/host" && systemReads.Add(1) == 3 {
						close(rechecking)
						<-releaseRecheck
					}
				}
				_ = json.NewEncoder(w).Encode(doc)
			}))
			DeferCleanup(func() { release(releaseRead); release(releaseRecheck); release(releaseCleanup); server.Close() })
			Expect(os.Setenv("DPF_PROGRESS_PASSWORD", "password-canary")).To(Succeed())
			DeferCleanup(os.Unsetenv, "DPF_PROGRESS_PASSWORD")
			source := fmt.Sprintf("credentials:\n  read: {username: username-canary, passwordRef: {env: DPF_PROGRESS_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{name: 'Named host', bmcEndpoint: '%s', credentialRef: read}]\n  probes: {mode: inventory}\n", server.URL)
			in, err := config.Decode([]byte(source), filepath.Join(GinkgoT().TempDir(), "site.yaml"))
			Expect(err).NotTo(HaveOccurred())
			req, err := config.Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			core, records := observer.New(zapcore.Level(-2))
			ctx, cancel := context.WithCancel(log.IntoContext(context.Background(), zapr.NewLogger(zap.New(core))))
			DeferCleanup(cancel)
			progress := &progressBuffer{}
			ctx = ui.WithOutput(ctx, ui.NewWithWriter(progress))
			type collected struct {
				device   inventory.Device
				evidence map[string]inventory.Evidence
				err      error
			}
			result := make(chan collected, 1)
			go func() {
				device, evidence, err := (Collector{}).Collect(ctx, req.Targets[0], 3*time.Second)
				result <- collected{device: device, evidence: evidence, err: err}
			}()
			Eventually(entered, 3*time.Second).Should(BeClosed())
			Expect(progress.String()).To(ContainSubstring("[RUNNING] \"Named host\""))
			Expect(progress.String()).To(ContainSubstring(": Physical inventory"))
			Expect(progress.String()).NotTo(ContainSubstring("[PASS] \"Named host\" (" + server.URL + "): Physical"))
			release(releaseRead)
			Eventually(rechecking, 3*time.Second).Should(BeClosed())
			if cleanupFails {
				Expect(progress.String()).To(ContainSubstring("[UNAVAILABLE]"))
			} else {
				Expect(progress.String()).NotTo(ContainSubstring("[UNAVAILABLE]"))
				Expect(progress.String()).To(ContainSubstring(": Optional BMC firmware/time/settings observations — reads completed; compatibility not evaluated"))
			}
			Expect(progress.String()).NotTo(ContainSubstring(": Physical identity recheck — verified"))
			release(releaseRecheck)
			Eventually(cleaning, 3*time.Second).Should(BeClosed())
			Expect(records.FilterMessage("Redfish collection completed").Len()).To(BeZero())
			Expect(progress.String()).NotTo(ContainSubstring(": Redfish session cleanup completed"))
			cancel() // Deferred cleanup has its own context and must still finish.
			release(releaseCleanup)
			var collection collected
			Eventually(result, 3*time.Second).Should(Receive(&collection))
			expectNoRedfishEvidence(collection.device, collection.evidence)
			collectionErr := collection.err
			if cleanupFails {
				Expect(collectionErr).To(HaveOccurred())
				Expect(progress.String()).To(ContainSubstring(": Redfish session cleanup (ACCESS_DENIED)"))
			} else {
				Expect(collectionErr).NotTo(HaveOccurred())
				Expect(progress.String()).To(ContainSubstring(": Redfish session cleanup completed"))
			}
			Expect(records.FilterMessage("Redfish collection completed").Len()).To(Equal(1))
			encoded, err := json.Marshal(records.All())
			Expect(err).NotTo(HaveOccurred())
			for _, secret := range []string{"password-canary", "username-canary", "token-canary", "session-id-canary", "response-body-canary"} {
				Expect(progress.String() + string(encoded)).NotTo(ContainSubstring(secret))
			}
		}
	})
})
