// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var asicTestPorts = []string{"swp1s0", "swp1s1", "swp19s0", "swp19s1"}

func asicFixture(name string) []byte {
	raw, err := os.ReadFile("testdata/asic/" + name)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return raw
}
func asicTestInputs() ASICInputs {
	in := ASICInputs{Before: ASICIdentityInputs{Root: asicFixture("root.txt"), Interfaces: asicFixture("interfaces.txt"), VRFs: asicFixture("vrfs.txt"), Linux: asicFixture("linux.json"), Config: asicFixture("config.json"), Pending: []byte(`{}`)}}
	in.After = in.Before
	var err error
	in.RIFs, err = ParseASICRIFDump(asicFixture("rifs.txt"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return in
}

var _ = Describe("qualified physical ASIC binding parsing", func() {
	It("joins every selected VPORT's actual SDK RouterID through observed maps", func() {
		in := asicTestInputs()
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports).To(HaveLen(4))
		Expect(got.Ports[1]).To(Equal(ASICPort{Name: "swp1s1", RIF: 223, RouterID: 6, RouterTable: 1006, LinuxVRF: "hbn", LinuxTable: 1006, MAC: "02:00:00:00:00:02", MTU: 9216, IPv4Enabled: true, IPv6Enabled: true}))
	})
	It("accepts a confirmed zero-byte successful pending diff while preserving nil as unknown", func() {
		in := asicTestInputs()
		in.Before.Pending = []byte{}
		in.After.Pending = []byte{}
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports).To(HaveLen(4))
		in.Before.Pending = nil
		got, err = ParseASICObservation(in, asicTestPorts)
		Expect(err).To(HaveOccurred())
		Expect(got).To(BeNil())
	})

	It("requires the exact qualified ordered NVUE header/set envelope and fences all of it", func() {
		valid := string(asicFixture("config.json"))
		for _, raw := range []string{
			`{"replay":"static-only"}`, `null`, `[]`, `[{"set":{"system":{"hostname":"switch-a"}}}]`,
			`[{"set":{"system":{"hostname":"switch-a"}}},{"header":{"model":"generic-switch","nvue-api-version":"1.0.0","rev-id":1.0,"version":"5.15.1"}}]`,
			strings.Replace(valid, `"model": "generic-switch"`, `"model": null`, 1),
			strings.Replace(valid, `"rev-id": 1.0`, `"rev-id": "1.0"`, 1),
			strings.Replace(valid, `"nvue-api-version": "1.0.0"`, `"unexpected": "1.0.0"`, 1),
			strings.Replace(valid, `"version": "5.15.1"`, `"version": ""`, 1),
			strings.Replace(valid, `"header": {`, `"header": null, "unexpected": {`, 1),
			`[{"header":{"model":"generic-switch","nvue-api-version":"1.0.0","rev-id":1.0,"version":"5.15.1"}},{"set":{}}]`,
			`[{"header":{"model":"generic-switch","nvue-api-version":"1.0.0","rev-id":1.0,"version":"5.15.1"}},{"set":{"system":null}}]`,
		} {
			in := asicTestInputs()
			in.Before.Config = []byte(raw)
			in.After.Config = in.Before.Config
			got, err := ParseASICObservation(in, asicTestPorts)
			Expect(err).To(HaveOccurred())
			Expect(got).To(BeNil())
		}
		in := asicTestInputs()
		in.After.Config = []byte(strings.Replace(valid, `"rev-id": 1.0`, `"rev-id": 2.0`, 1))
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).To(HaveOccurred())
		Expect(got).To(BeNil())
		in = asicTestInputs()
		in.After.Config = []byte(strings.ReplaceAll(valid, "\n", " "))
		got, err = ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports).To(HaveLen(4))
	})

	It("retains a direct RouterID0/table0 mismatch while software still says hbn", func() {
		in := asicTestInputs()
		raw := strings.Replace(string(asicFixture("rifs.txt")), "|   223|         6|", "|   223|         0|", 1)
		var err error
		in.RIFs, err = ParseASICRIFDump([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports[1].RouterTable).To(Equal(uint32(0)))
		Expect(got.Ports[1].LinuxVRF).To(Equal("hbn"))
		Expect(got.Ports[1].LinuxTable).To(Equal(uint32(1006)))
	})
	It("uses explicit non-default table/VRID mappings without arithmetic", func() {
		in := asicTestInputs()
		in.Before.Linux = []byte(strings.ReplaceAll(strings.ReplaceAll(string(in.Before.Linux), "hbn", "custom"), "1006", "9012"))
		in.After.Linux = in.Before.Linux
		in.Before.Interfaces = []byte(strings.ReplaceAll(string(in.Before.Interfaces), "table-id 1006", "table-id 9012"))
		in.After.Interfaces = in.Before.Interfaces
		var err error
		in.RIFs, err = ParseASICRIFDump([]byte(strings.ReplaceAll(string(asicFixture("rifs.txt")), "|         6|", "|        37|")))
		Expect(err).NotTo(HaveOccurred())
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports[0].RouterTable).To(Equal(uint32(9012)))
		Expect(got.Ports[0].LinuxVRF).To(Equal("custom"))
	})
	It("maps the qualified no-master default sentinel without assuming main254 or a VRID offset", func() {
		in := asicTestInputs()
		in.Before.Linux = []byte(strings.ReplaceAll(string(in.Before.Linux), `, "master": "hbn"`, ""))
		in.After.Linux = in.Before.Linux
		in.Before.Interfaces = []byte(strings.ReplaceAll(string(in.Before.Interfaces), "table-id 1006", "table-id 0"))
		in.After.Interfaces = in.Before.Interfaces
		var err error
		in.RIFs, err = ParseASICRIFDump([]byte(strings.ReplaceAll(string(asicFixture("rifs.txt")), "|         6|", "|         0|")))
		Expect(err).NotTo(HaveOccurred())
		got, err := ParseASICObservation(in, asicTestPorts)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Ports[0].LinuxVRF).To(Equal("default"))
		Expect(got.Ports[0].LinuxTable).To(BeZero())
		Expect(got.Ports[0].RouterTable).To(BeZero())
	})
	It("rejects missing, malformed, duplicate, changed and unsupported identities", func() {
		for _, test := range []struct {
			name string
			edit func(*ASICInputs)
		}{
			{"pending changes", func(i *ASICInputs) { i.Before.Pending = []byte(`{"set":{"x":1}}`) }},
			{"unmeasured pending read", func(i *ASICInputs) { i.Before.Pending = nil }},
			{"whitespace is not a confirmed empty diff", func(i *ASICInputs) { i.Before.Pending = []byte(" \n") }},
			{"malformed nonempty pending output", func(i *ASICInputs) { i.Before.Pending = []byte("Failed to read pending configuration") }},

			{"missing config", func(i *ASICInputs) { i.Before.Config = nil }},
			{"changed config", func(i *ASICInputs) {
				i.After.Config = []byte(strings.Replace(string(i.After.Config), "switch-a", "switch-b", 1))
			}},
			{"duplicate config key", func(i *ASICInputs) {
				i.Before.Config = []byte(strings.Replace(string(i.Before.Config), `"hostname": "switch-a"`, `"hostname": "switch-a", "hostname": "switch-b"`, 1))
			}},
			{"changed table identity", func(i *ASICInputs) {
				i.After.Root = []byte(strings.ReplaceAll(string(i.After.Root), "00000011", "00000012"))
			}},
			{"root traversal", func(i *ASICInputs) {
				i.Before.Root = []byte(strings.ReplaceAll(string(i.Before.Root), "00000011", "../../11"))
			}},
			{"duplicate root name", func(i *ASICInputs) {
				i.Before.Root = []byte(strings.ReplaceAll(string(i.Before.Root), "Backend VRF Table", "Backend Interface Table"))
			}},
			{"backend header identity mismatch", func(i *ASICInputs) {
				i.Before.Interfaces = []byte(strings.Replace(string(i.Before.Interfaces), "TableId: 11", "TableId: 12", 1))
			}},
			{"null master is not default", func(i *ASICInputs) {
				i.Before.Linux = []byte(strings.ReplaceAll(string(i.Before.Linux), `"master": "hbn"`, `"master": null`))
				i.After.Linux = i.Before.Linux
			}},

			{"missing interface", func(i *ASICInputs) {
				i.Before.Interfaces = []byte(strings.ReplaceAll(string(i.Before.Interfaces), "swp1s1", "swp9s1"))
			}},
			{"changed Linux identity", func(i *ASICInputs) {
				i.After.Linux = []byte(strings.ReplaceAll(string(i.After.Linux), "00:00:00:00:02", "00:00:00:00:09"))
			}},
			{"duplicate Linux key", func(i *ASICInputs) {
				i.Before.Linux = []byte(strings.Replace(string(i.Before.Linux), `"ifname": "swp1s0"`, `"ifname": "swp1s0", "ifname": "swp9"`, 1))
			}},
			{"unmapped master", func(i *ASICInputs) {
				i.Before.Linux = []byte(strings.ReplaceAll(string(i.Before.Linux), `"master": "hbn"`, `"master": "absent"`))
				i.After.Linux = i.Before.Linux
			}},
			{"truncated backend", func(i *ASICInputs) { i.Before.Interfaces = i.Before.Interfaces[:len(i.Before.Interfaces)-6] }},
			{"duplicate backend field", func(i *ASICInputs) {
				i.Before.Interfaces = []byte(strings.Replace(string(i.Before.Interfaces), "     table-id 1006", "     table-id 1006\n     table-id 1006", 1))
			}},
			{"duplicate VRID", func(i *ASICInputs) {
				i.Before.VRFs = []byte(strings.Replace(string(i.Before.VRFs), "vr-id 00000037", "vr-id 00000006", 1))
			}},
			{"incomplete VRF count", func(i *ASICInputs) {
				i.Before.VRFs = []byte(strings.Replace(string(i.Before.VRFs), "Count: 3", "Count: 4", 1))
			}},
			{"changed backend map", func(i *ASICInputs) { i.After.VRFs = []byte(strings.ReplaceAll(string(i.After.VRFs), "1006", "1009")) }},
			{"no SDK read", func(i *ASICInputs) { i.RIFs = nil }},
		} {
			By(test.name)
			in := asicTestInputs()
			test.edit(&in)
			got, err := ParseASICObservation(in, asicTestPorts)
			Expect(err).To(HaveOccurred(), test.name)
			Expect(got).To(BeNil())
		}
	})
	It("rejects successful-exit SDK errors and invalid complete tables", func() {
		raw := string(asicFixture("rifs.txt"))
		firstRow := strings.Split(raw, "\n")[11]
		for _, bad := range []string{
			"The router module is not initialized. Dump is not available.\n",
			raw + "Failed to set router API verbosity level\n",
			strings.Replace(raw, "Router ID", "RouterID", 1),
			strings.Replace(raw, "|   223|", "|   219|", 1),
			strings.Replace(raw, "|   223|", "|   xyz|", 1),
			raw + firstRow + "\n",
			strings.TrimRight(raw, "\n-") + "\n",
			"Traceback (most recent call last):\n" + raw,
		} {
			got, err := ParseASICRIFDump([]byte(bad))
			Expect(err).To(HaveOccurred())
			Expect(got).To(BeNil())
		}
	})
	It("does not pass missing selected SDK rows, unknown UC state, unsupported RIF type or changed MAC/port", func() {
		raw := string(asicFixture("rifs.txt"))
		for _, bad := range []string{
			strings.Replace(raw, "|   223|", "|   999|", 1),
			strings.Replace(raw, "|   223|         6|      VPORT|", "|   223|         6|  PORT_VLAN|", 1),
			strings.Replace(raw, "0x2e8d00f3", "0x2e8d00f4", 1),
			strings.Replace(raw, "02:00:00:00:00:02", "02:00:00:00:00:09", 1),
			strings.Replace(raw, "|       ENABLE|       ENABLE|", "|       UNKOWN|       ENABLE|", 1),
		} {
			in := asicTestInputs()
			var err error
			in.RIFs, err = ParseASICRIFDump([]byte(bad))
			Expect(err).NotTo(HaveOccurred())
			got, err := ParseASICObservation(in, asicTestPorts)
			Expect(err).To(HaveOccurred())
			Expect(got).To(BeNil())
		}
	})
})
