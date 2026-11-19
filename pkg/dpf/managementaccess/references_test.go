// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementaccess_test

import (
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

var _ = Describe("management input file ownership", func() {
	It("includes unused contexts and missing credential files without opening them", func() {
		dir := GinkgoT().TempDir()
		filename := filepath.Join(dir, "kubeconfig")
		raw := clientapi.Config{
			CurrentContext: "selected", Contexts: map[string]*clientapi.Context{"selected": {Cluster: "selected", AuthInfo: "selected"}},
			Clusters:  map[string]*clientapi.Cluster{"selected": {Server: "https://api.example.test", CertificateAuthority: "selected-ca"}, "unused": {Server: "https://unused.example.test", CertificateAuthority: "unused-ca"}},
			AuthInfos: map[string]*clientapi.AuthInfo{"selected": {Token: "private-test-token"}, "unused": {ClientCertificate: "unused-cert", ClientKey: "unused-key", TokenFile: "unused-token"}},
		}
		data, err := clientcmd.Write(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filename, data, 0600)).To(Succeed())
		files, err := managementaccess.ReferencedFiles(filename)
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(ConsistOf(filename, filepath.Join(dir, "selected-ca"), filepath.Join(dir, "unused-ca"), filepath.Join(dir, "unused-cert"), filepath.Join(dir, "unused-key"), filepath.Join(dir, "unused-token")))
		Expect(string(data)).To(ContainSubstring("private-test-token"))
		for _, filename := range files {
			Expect(filename).NotTo(ContainSubstring("private-test-token"))
		}
	})

	It("does not claim complete local ownership for opaque authentication plugins", func() {
		filename := writeKubeconfig(GinkgoT().TempDir(), &clientapi.Cluster{Server: "https://api.example.test"}, &clientapi.AuthInfo{Exec: &clientapi.ExecConfig{Command: "/must-not-execute"}})
		files, err := managementaccess.ReferencedFiles(filename)
		Expect(err).To(MatchError("cannot enumerate local file references in kubeconfig authentication plugins"))
		Expect(files).To(BeNil())
	})
})
