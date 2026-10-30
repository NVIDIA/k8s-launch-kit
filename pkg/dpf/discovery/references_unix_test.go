//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var _ = Describe("bounded reference files", func() {
	It("rejects a kubeconfig FIFO before a blocking read", func() {
		dir, e := os.MkdirTemp("", "dpf-fifo-")
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		filename := filepath.Join(dir, "pipe")
		Expect(syscall.Mkfifo(filename, 0600)).To(Succeed())
		done := make(chan error, 1)
		go func() { _, err := ManagementAccess(config.Request{Kubeconfig: filename}); done <- err }()
		Eventually(done, time.Second).Should(Receive(HaveOccurred()))
	})
})

var _ = Describe("unambiguous credential references", func() {
	It("rejects an embedded token plus FIFO tokenFile before any read", func() {
		dir, e := os.MkdirTemp("", "dpf-token-")
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		fifo := filepath.Join(dir, "token")
		Expect(syscall.Mkfifo(fifo, 0600)).To(Succeed())
		raw := clientapi.Config{CurrentContext: "m", Clusters: map[string]*clientapi.Cluster{"m": {Server: "https://example.invalid"}}, AuthInfos: map[string]*clientapi.AuthInfo{"a": {Token: "inline-token", TokenFile: fifo}}, Contexts: map[string]*clientapi.Context{"m": {Cluster: "m", AuthInfo: "a"}}}
		b, e := clientcmd.Write(raw)
		Expect(e).NotTo(HaveOccurred())
		filename := filepath.Join(dir, "config")
		Expect(os.WriteFile(filename, b, 0600)).To(Succeed())
		done := make(chan error, 1)
		go func() { _, err := ManagementAccess(config.Request{Kubeconfig: filename}); done <- err }()
		Eventually(done, time.Second).Should(Receive(HaveOccurred()))
	})
})
