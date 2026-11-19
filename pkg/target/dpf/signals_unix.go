//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package dpf

import (
	"os"
	"os/signal"
	"syscall"
)

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// Keep a closed stdout/stderr pipe on the checked writer-error path while DPF
// validation runs. Stop restores the previous signal handling after the operation.
func notifyValidationBrokenPipe() func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGPIPE)
	return func() { signal.Stop(signals) }
}
