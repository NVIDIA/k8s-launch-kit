// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("probe TLS policy", func() {
	It("serializes an explicit per-destination bypass without credential material", func() {
		files, err := requestFiles(tlsPolicyRequest(Destination{
			ID: "dpu-target", Endpoint: "https://192.0.2.20", Path: "/redfish/v1/", InsecureSkipVerify: true,
		}))
		Expect(err).NotTo(HaveOccurred())

		var request struct {
			Destinations []struct {
				ID                 string `json:"id"`
				InsecureSkipVerify bool   `json:"insecureSkipVerify"`
			} `json:"destinations"`
		}
		Expect(json.Unmarshal([]byte(files["request.json"]), &request)).To(Succeed())
		Expect(request.Destinations).To(HaveLen(1))
		Expect(request.Destinations[0].ID).To(Equal("dpu-target"))
		Expect(request.Destinations[0].InsecureSkipVerify).To(BeTrue())
		Expect(files["request.json"]).NotTo(ContainSubstring("username"))
		Expect(files["request.json"]).NotTo(ContainSubstring("password"))
		Expect(files["request.json"]).NotTo(ContainSubstring("credential"))
	})

	It("keeps the default destination strict", func() {
		files, err := requestFiles(tlsPolicyRequest(Destination{
			ID: "dpu-target", Endpoint: "https://192.0.2.20", Path: "/redfish/v1/",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(files["request.json"]).NotTo(ContainSubstring(`"insecureSkipVerify":true`))
	})

	It("rejects skipping verification for the management API", func() {
		_, err := requestFiles(tlsPolicyRequest(Destination{
			ID: "management-api", Endpoint: "https://api.example.invalid", Path: "/version", InsecureSkipVerify: true,
		}))
		Expect(err).To(MatchError(ContainSubstring("not allowed for the management API")))
	})

	It("rejects a bypass combined with a CA bundle", func() {
		_, err := requestFiles(tlsPolicyRequest(Destination{
			ID: "dpu-target", Endpoint: "https://192.0.2.20", Path: "/redfish/v1/", CA: []byte("not a CA"), InsecureSkipVerify: true,
		}))
		Expect(err).To(MatchError(ContainSubstring("or with a CA")))
	})
})

func tlsPolicyRequest(destination Destination) Request {
	return Request{
		Version: ProtocolVersion, RunID: "tls-policy-test", Deadline: time.Now().Add(time.Minute),
		RequestTimeout: time.Second, Destinations: []Destination{destination},
	}
}

var _ = Describe("probe TLS policy input boundaries", func() {
	It("continues to reject control characters in TLS identity inputs", func() {
		request := tlsPolicyRequest(Destination{
			ID: "dpu-target", Endpoint: "https://192.0.2.20", Path: "/redfish/v1/", ServerName: "bmc.invalid\r\nInjected: yes",
		})
		_, err := requestFiles(request)
		Expect(err).To(HaveOccurred())
		Expect(strings.Contains(err.Error(), "TLS server name")).To(BeTrue())
	})
})
