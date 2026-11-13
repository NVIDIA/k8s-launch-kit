// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
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

type recheckBlocker struct {
	remote           runner
	entered, release chan struct{}
}

func (b recheckBlocker) Run(ctx context.Context, stage, command string) (string, error) {
	if stage == "arm-dmi-recheck" {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: stage}
		}
	}
	return b.remote.Run(ctx, stage, command)
}

var _ = Describe("secret-safe SSH stages", func() {
	It("traces command metadata without command arguments, remote output, trust or credentials", func() {
		server := startServer()
		DeferCleanup(server.close)
		for _, level := range []zapcore.Level{zapcore.DebugLevel, -2} {
			core, records := observer.New(level)
			ctx := log.IntoContext(context.Background(), zapr.NewLogger(zap.New(core)))
			c, err := connect(ctx, "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Run(ctx, "host-vpd:private-id-canary", "query argument-canary")
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Run(ctx, "firmware", "fail")
			Expect(Code(err)).To(Equal("SSH_COMMAND_FAILED"))
			_ = c.Close()
			if level == -2 {
				stages := records.FilterMessage("SSH command stage completed").All()
				Expect(stages).To(HaveLen(2))
				Expect(stages[0].ContextMap()).To(HaveKeyWithValue("stage", "host-vpd"))
				Expect(stages[1].ContextMap()).To(HaveKeyWithValue("reasonCode", "SSH_COMMAND_FAILED"))
				Expect(stages[1].ContextMap()).To(HaveKey("duration"))
			} else {
				Expect(records.Len()).To(BeZero())
			}
			encoded, err := json.Marshal(records.All())
			Expect(err).NotTo(HaveOccurred())
			for _, secret := range []string{"test-password", "reader", "private-id-canary", "argument-canary", "remote sensitive diagnostic", "warning\\n", "observed\\n"} {
				Expect(string(encoded)).NotTo(ContainSubstring(secret))
			}
		}
	})
	It("shows optional gaps and withholds identity success until a blocked recheck finishes", func() {
		remote := recheckBlocker{remote: armRunner(), entered: make(chan struct{}), release: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		DeferCleanup(cancel)
		progress := &progressBuffer{}
		ctx = ui.WithOutput(ctx, ui.NewWithWriter(progress))
		result := make(chan error, 1)
		go func() {
			_, _, err := collect(ctx, config.Target{Name: "DPU ARM", Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, remote)
			result <- err
		}()
		Eventually(remote.entered).Should(BeClosed())
		Expect(progress.String()).To(ContainSubstring("[RUNNING] \"DPU ARM\""))
		Expect(progress.String()).To(ContainSubstring("[UNAVAILABLE]"))
		Expect(progress.String()).To(ContainSubstring(": Physical identity recheck"))
		Expect(progress.String()).NotTo(ContainSubstring(": Physical identity recheck verified"))
		close(remote.release)
		var err error
		Eventually(result).Should(Receive(&err))
		Expect(err).NotTo(HaveOccurred())
		Expect(progress.String()).To(ContainSubstring(": Physical identity recheck verified"))
	})
	It("counts missing nested card facts independently of successful tool reads", func() {
		runtime := &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{"tool": {State: "observed"}}, Hardware: &inventory.HardwareObservation{RShim: inventory.Observation{ReasonCode: "NOT_APPLICABLE"}, Cards: []inventory.CardHardware{{Firmware: inventory.Observation{State: "observed"}, PSID: inventory.Observation{State: "unknown"}, Privilege: inventory.Observation{ReasonCode: "NOT_APPLICABLE"}, NVConfig: map[string]inventory.Observation{"mode": {State: "unavailable"}}}}}}
		observed, unavailable := optionalCounts(runtime)
		Expect(observed).To(Equal(2))
		Expect(unavailable).To(Equal(2))
	})
})
