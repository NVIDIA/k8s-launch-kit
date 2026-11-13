// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
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

type progressCollector struct {
	entered chan string
	release chan struct{}
	fail    bool
}

func (c progressCollector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	ui.FromContext(ctx).Info("[RUNNING] %s: fixture observation", target.DisplayName())
	c.entered <- target.Role
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return (fixtureCollector{admin: true, fail: c.fail}).Collect(ctx, target, timeout)
}

var _ = Describe("discovery progress and diagnostic context", func() {
	It("streams concurrent named sources, respects quiet output and keeps logs secret-safe", func() {
		for _, tc := range []struct{ quiet, fail bool }{{false, false}, {false, true}, {true, false}} {
			var progress bytes.Buffer
			core, records := observer.New(zapcore.DebugLevel)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ctx = log.IntoContext(ctx, zapr.NewLogger(zap.New(core)))
			ctx = ui.WithOutput(ctx, ui.NewWithWriter(&progress))
			if tc.quiet {
				ctx = ui.WithOutput(ctx, ui.NewSilent())
			}
			req := inventoryRequest()
			req.Timeout = 3 * time.Second
			req.Targets[0].Name = "Host A"
			req.Targets[0].Endpoint = "https://url-user-canary:url-pass-canary@host.invalid/private-path-canary?token=query-canary"
			req.Targets[1].Name, req.Targets[1].Endpoint, req.Targets[1].SSH = "DPU A", "https://dpu.invalid", nil
			collector := progressCollector{entered: make(chan string, 2), release: make(chan struct{}), fail: tc.fail}
			result := make(chan inventory.Report, 1)
			go func() { result <- Collect(ctx, req, "private-run-id-canary", Dependencies{BMC: collector}) }()
			Eventually(collector.entered).Should(Receive())
			Eventually(collector.entered).Should(Receive())
			// Both goroutines are blocked; these lines cannot be a post-run replay.
			if !tc.quiet {
				Expect(progress.String()).To(ContainSubstring(`[RUNNING] "Host A" (https://host.invalid): fixture observation`))
				Expect(progress.String()).To(ContainSubstring(`[RUNNING] "DPU A" (https://dpu.invalid): fixture observation`))
				Expect(progress.String()).NotTo(ContainSubstring("Required discovery checks completed"))
			}
			close(collector.release)
			var report inventory.Report
			Eventually(result).Should(Receive(&report))
			cancel()
			Expect(records.FilterMessage("Endpoint observation started").Len()).To(Equal(2))
			Expect(records.FilterMessage("Endpoint observation completed").Len()).To(Equal(2))
			Expect(records.FilterMessage("Discovery check evaluated").Len()).To(BeNumerically(">", 0))
			Expect(records.FilterMessage("DPF discovery completed").Len()).To(Equal(1))
			if tc.quiet {
				Expect(progress.String()).To(BeEmpty())
			} else {
				for _, line := range strings.Split(strings.TrimSpace(progress.String()), "\n") {
					Expect(line).To(MatchRegexp(`^\[(RUNNING|PASS|FAIL|UNAVAILABLE|SKIP)\] `))
				}
				Expect(progress.String()).NotTo(ContainSubstring("QUALIFIED_ADAPTER_REQUIRED"))
			}
			if tc.fail {
				Expect(report.Discovery.Outcome).To(Equal("partial"))
				Expect(progress.String()).To(ContainSubstring("[FAIL] Required discovery checks"))
			} else {
				Expect(report.Discovery.Outcome).To(Equal("complete"))
			}
			encoded, err := json.Marshal(records.All())
			Expect(err).NotTo(HaveOccurred())
			for _, secret := range []string{"url-user-canary", "url-pass-canary", "private-path-canary", "query-canary", "private-run-id-canary"} {
				Expect(progress.String() + string(encoded)).NotTo(ContainSubstring(secret))
			}
		}
	})
})
