// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

var _ = Describe("probe prerequisites before remote effects", func() {
	assets := func() Assets {
		return Assets{Repository: "example.test/probe", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlatformDigests: map[string]string{"amd64": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Qualified: true}
	}
	It("rejects a later unsupported node before reading or creating any Kubernetes resource", func() {
		for _, scenario := range []string{"architecture", "os", "not-ready", "unschedulable"} {
			client := fake.NewClientset()
			a := assets()
			runner := Runner{Assets: a, Journal: func([]Receipt) error { return nil }}
			nodes := []inventory.Node{{Name: "first", UID: "first-uid", Ready: true, OS: "linux", Architecture: "amd64"}, {Name: "last", UID: "last-uid", Ready: true, OS: "linux", Architecture: "amd64"}}
			switch scenario {
			case "architecture":
				nodes[1].Architecture = "arm64"
			case "os":
				nodes[1].OS = "windows"
			case "not-ready":
				nodes[1].Ready = false
			case "unschedulable":
				nodes[1].Unschedulable = true
			}
			rows, cleanup, err := runner.Run(context.Background(), config.Request{Timeout: time.Minute, RequestTimeout: time.Second}, managementaccess.Client{Client: client, REST: &rest.Config{}, Namespace: "default"}, &inventory.Inventory{Management: &inventory.Management{Nodes: nodes}})
			Expect(err).To(MatchError(ContainSubstring("management node cannot")))
			Expect(rows).To(BeEmpty())
			Expect(cleanup.State).To(Equal("complete"))
			Expect(client.Actions()).To(BeEmpty(), scenario)
		}
	})
	It("rejects absent or malformed public inputs without Kubernetes operations", func() {
		for _, scenario := range []string{"context", "client", "rest", "inventory", "management", "nodes", "endpoint"} {
			client := fake.NewClientset()
			runner := Runner{Assets: assets(), Journal: func([]Receipt) error { return nil }}
			ctx := context.Background()
			management := managementaccess.Client{Client: client, REST: &rest.Config{Host: "https://management.example"}, Namespace: "default"}
			inv := &inventory.Inventory{Management: &inventory.Management{Nodes: []inventory.Node{{Name: "n", UID: "u", Ready: true, OS: "linux", Architecture: "amd64"}}}}
			switch scenario {
			case "context":
				ctx = nil
			case "client":
				management.Client = nil
			case "rest":
				management.REST = nil
			case "inventory":
				inv = nil
			case "management":
				inv.Management = nil
			case "nodes":
				inv.Management.Nodes = nil
			case "endpoint":
				management.REST.Host = "https://%gh"
			}
			_, _, err := runner.Run(ctx, config.Request{Timeout: time.Minute, RequestTimeout: time.Second}, management, inv)
			Expect(err).To(HaveOccurred(), scenario)
			Expect(client.Actions()).To(BeEmpty(), scenario)
		}
	})
})
