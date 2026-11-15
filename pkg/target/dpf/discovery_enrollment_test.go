// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func enrollmentAccessRequest(username, secondPassword string) (config.Request, *inventory.Inventory) {
	GinkgoHelper()
	GinkgoT().Setenv("HBN_ENROLLMENT_FIRST", "hbn-credential-canary-one")
	GinkgoT().Setenv("HBN_ENROLLMENT_SECOND", secondPassword)
	input, err := config.Decode([]byte(fmt.Sprintf(`dpf:
  useCase: hbn-bgp
  release: 26.8.0
credentials:
  first: {username: %s, passwordRef: {env: HBN_ENROLLMENT_FIRST}}
  second: {username: admin, passwordRef: {env: HBN_ENROLLMENT_SECOND}}
discovery:
  probes: {mode: inventory}
  dpus:
    - {bmcEndpoint: 'https://192.0.2.30', credentialRef: first}
    - {bmcEndpoint: 'https://192.0.2.31', credentialRef: second}
  hosts:
    - {bmcEndpoint: 'https://192.0.2.90', credentialRef: second}
`, username)), "/site/input.yaml")
	Expect(err).NotTo(HaveOccurred())
	request, err := config.Resolve(input)
	Expect(err).NotTo(HaveOccurred())
	selected := generationSite()
	selected.Site.Discovery.DPUs = append(selected.Site.Discovery.DPUs, config.Endpoint{BMCEndpoint: "https://192.0.2.31", CredentialRef: "second"})
	selected.Site.Discovery.Hosts = append(selected.Site.Discovery.Hosts, config.Endpoint{SSH: &config.SSH{Address: "192.0.2.41"}, CredentialRef: "host"})
	current := selected.Site.Inventory
	Expect(request.Software.Version).To(Equal(current.Software.Version))
	Expect(current.DPUs).To(HaveLen(1))
	Expect(current.Hosts).To(HaveLen(1))
	second := current.DPUs[0]
	second.ID, second.InputRef, second.BMCEndpoint, second.SerialNumber = "dpu-b", "/discovery/dpus/1", "https://192.0.2.31", "SERIAL-B"
	second.Observations = maps.Clone(second.Observations)
	current.DPUs = append(current.DPUs, second)
	host := current.Hosts[0]
	host.ID, host.InputRef, host.SSHEndpoint = "host-b", "/discovery/hosts/1", "ssh://192.0.2.41:22"
	host.Adapters = []inventory.Adapter{{Key: "adapter-b", IdentityState: "known", SerialNumber: "SERIAL-B", PCIAddress: "0000:02:00.0"}}
	current.Hosts = append(current.Hosts, host)
	current.Associations = append(current.Associations, inventory.Association{DPUID: "dpu-b", HostID: "host-b", HostAdapterKey: "adapter-b", SerialNumber: "SERIAL-B"})
	selected.Site.Inventory = nil // The validator must use this run's explicit current facts.
	request.Input = selected
	intent, err := config.ResolveIntent(selected.Site)
	Expect(err).NotTo(HaveOccurred())
	request.HBN = intent.Networking
	return request, current
}

var _ = Describe("HBN enrollment credential compatibility", func() {
	It("accepts separate aliases with identical values without rereading references or mutating selection", func() {
		request, current := enrollmentAccessRequest("admin", "hbn-credential-canary-one")
		before, err := json.Marshal(current)
		Expect(err).NotTo(HaveOccurred())
		// Resolved access is authoritative even after its original values disappear.
		Expect(os.Unsetenv("HBN_ENROLLMENT_FIRST")).To(Succeed())
		Expect(os.Unsetenv("HBN_ENROLLMENT_SECOND")).To(Succeed())
		check := hbnEnrollmentAccessCheck(request, current)
		Expect(check.State).To(Equal("passed"))
		Expect(check.ReasonCode).To(Equal("ENROLLMENT_CREDENTIALS_COMPATIBLE"))
		after, err := json.Marshal(current)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		Expect(request.Input.Site.Inventory).To(BeNil())
	})

	DescribeTable("rejects incompatible selected runtime bindings despite Administrator inventory proof", func(username, secondPassword string, removeTargets bool) {
		request, current := enrollmentAccessRequest(username, secondPassword)
		if removeTargets {
			request.Targets = nil
		}
		check := hbnEnrollmentAccessCheck(request, current)
		Expect(check.State).To(Equal("failed"))
		Expect(check.ReasonCode).To(Equal("ENROLLMENT_CREDENTIALS_INCOMPATIBLE"))
		encoded, err := json.Marshal(check)
		Expect(err).NotTo(HaveOccurred())
		encodedYAML, err := yaml.Marshal(check)
		Expect(err).NotTo(HaveOccurred())
		for _, password := range []string{"hbn-credential-canary-one", secondPassword} {
			for _, forbidden := range []string{password, fmt.Sprintf("%x", sha256.Sum256([]byte(password)))} {
				Expect(string(encoded) + string(encodedYAML)).NotTo(ContainSubstring(forbidden))
			}
		}
		Expect(current.DPUs).To(HaveLen(2))
		for _, dpu := range current.DPUs {
			Expect(dpu.Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
		}
	},
		Entry("different Administrator username", "site-maintainer", "hbn-credential-canary-one", false),
		Entry("unequal password values", "admin", "hbn-credential-canary-two", false),
		Entry("missing resolved access", "admin", "hbn-credential-canary-one", true),
	)

	It("keeps incomplete current identities unknown without exposing raw selected values", func() {
		request, current := enrollmentAccessRequest("admin", "hbn-credential-canary-one")
		current.DPUs[0].PSID = ""
		check := hbnEnrollmentAccessCheck(request, current)
		Expect(check.State).To(Equal("unknown"))
		Expect(check.ReasonCode).To(Equal("ENROLLMENT_IDENTITY_UNAVAILABLE"))
		Expect(check.Message).To(BeEmpty())
		Expect(hbnEnrollmentAccessCheck(request, nil).State).To(Equal("unknown"))
	})

	It("rejects missing or unreadable references during resolution before a validator or collector can run", func() {
		for _, reference := range []string{"{env: HBN_ENROLLMENT_MISSING}", "{file: /must-not-open-missing-hbn-enrollment-password}"} {
			GinkgoT().Setenv("HBN_ENROLLMENT_MISSING", "")
			input, err := config.Decode([]byte(strings.ReplaceAll(`credentials:
  bmc: {username: admin, passwordRef: REFERENCE}
discovery:
  probes: {mode: inventory}
  hosts: [{bmcEndpoint: 'https://192.0.2.90', credentialRef: bmc}]
  dpus: [{bmcEndpoint: 'https://192.0.2.30', credentialRef: bmc}]
`, "REFERENCE", reference)), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			_, err = config.Resolve(input)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("hbn-credential-canary"))
		}
	})
})
