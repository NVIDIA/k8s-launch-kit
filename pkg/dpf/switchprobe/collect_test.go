// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package switchprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSwitchProbe(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPF switch discovery")
}

type commandCall struct {
	command string
	input   []byte
	stdin   bool
}

type fakeSession struct {
	calls     []commandCall
	outputs   map[string]string
	failures  map[string]error
	onRun     func(context.Context, string)
	closeErr  error
	closeSeen bool
}

func (s *fakeSession) Run(ctx context.Context, stage, command string) (string, error) {
	s.calls = append(s.calls, commandCall{command: command})
	if s.onRun != nil {
		s.onRun(ctx, command)
	}
	if err := ctx.Err(); err != nil {
		return "", &sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: stage}
	}
	if err := s.failures[wrappedCommand(command)]; err != nil {
		return "", err
	}
	return s.outputs[wrappedCommand(command)], nil
}

func (s *fakeSession) RunWithInput(ctx context.Context, stage, command string, input []byte) (string, error) {
	s.calls = append(s.calls, commandCall{command: command, input: append([]byte(nil), input...), stdin: true})
	if s.onRun != nil {
		s.onRun(ctx, command)
	}
	if err := ctx.Err(); err != nil {
		return "", &sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: stage}
	}
	if err := s.failures[wrappedCommand(command)]; err != nil {
		return "", err
	}
	return s.outputs[wrappedCommand(command)], nil
}

func (s *fakeSession) Close() error {
	s.closeSeen = true
	return s.closeErr
}

func wrappedCommand(command string) string {
	if strings.HasPrefix(command, "sudo -S -p '' -- ") {
		return strings.TrimPrefix(command, "sudo -S -p '' -- ")
	}
	if strings.HasPrefix(command, "sudo -n -- ") {
		return strings.TrimPrefix(command, "sudo -n -- ")
	}
	return command
}

func testSwitchTarget(password string, usePrivateKey bool) config.SwitchTarget {
	dir := GinkgoT().TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	signer, err := ssh.NewSignerFromKey(private)
	Expect(err).NotTo(HaveOccurred())
	knownHosts := knownhosts.Line([]string{"switch.example"}, signer.PublicKey()) + "\n"
	Expect(os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownHosts), 0600)).To(Succeed())

	previous, existed := os.LookupEnv("L8K_SWITCHPROBE_TEST_PASSWORD")
	if err := os.Setenv("L8K_SWITCHPROBE_TEST_PASSWORD", password); err != nil {
		Fail(err.Error())
	}
	DeferCleanup(func() {
		if existed {
			Expect(os.Setenv("L8K_SWITCHPROBE_TEST_PASSWORD", previous)).To(Succeed())
		} else {
			Expect(os.Unsetenv("L8K_SWITCHPROBE_TEST_PASSWORD")).To(Succeed())
		}
	})

	credential := "reader: {username: reader, passwordRef: {env: L8K_SWITCHPROBE_TEST_PASSWORD}}"
	if usePrivateKey {
		block, err := ssh.MarshalPrivateKey(private, "switchprobe test")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, "key"), pem.EncodeToMemory(block), 0600)).To(Succeed())
		credential = "reader: {username: reader, privateKeyRef: {file: key}}"
	}
	document := strings.Join([]string{
		"dpf:",
		"  useCase: hbn-bgp",
		"credentials:",
		"  " + credential,
		"discovery:",
		"  hosts:",
		"    - credentialRef: reader",
		"      ssh: {address: host.example, knownHostsFile: known_hosts}",
		"  switches:",
		"    - name: switch-a",
		"      credentialRef: reader",
		"      ssh: {address: switch.example, knownHostsFile: known_hosts, sudo: true}",
		"      ports:",
		"        - {name: swp1, dpuSerial: dpu-serial-1, dpuInterface: p0}",
		"  probes: {mode: inventory}",
	}, "\n")
	in, err := config.Decode([]byte(document), filepath.Join(dir, "site.yaml"))
	Expect(err).NotTo(HaveOccurred())
	resolved, err := config.Resolve(in)
	Expect(err).NotTo(HaveOccurred())
	Expect(resolved.Switches).To(HaveLen(1))
	return resolved.Switches[0]
}

func validOutputs() map[string]string {
	return map[string]string{
		"nv config revision":                           validRevisionTable(),
		"nv show interface swp1 --rev applied -o json": `{"type":"swp","vrf":"default","link":{"mtu":9216}}`,
		"nv show interface swp1 -o json":               `{"type":"swp","vrf":"default","parent":"default","link":{"admin-status":"up","oper-status":"up","mtu":9216}}`,
		"nv show interface --rev applied -o json":      `{"swp1":{"type":"swp","link":{"mtu":9216}}}`,
		"nv show vrf --rev applied -o json":            `{"default":{"router":{"bgp":{"autonomous-system":4200000000}}}}`,
		"nv show router --rev applied -o json":         `{"bgp":{"autonomous-system":4200000000}}`,
		"nv show bridge --rev applied -o json":         `{"domain":{}}`,
		"nv show interface -o json":                    `{"swp1":{"type":"swp","link":{"admin-status":"up","oper-status":"up","mtu":9216}}}`,
		"nv show bridge -o json":                       `{"domain":{}}`,
		"ip -j nexthop show":                           `[]`,
		"ip -j address show":                           `[]`,
		"ip -j -4 route show table all":                `[]`,
		"ip -j -6 route show table all":                `[]`,
		"/usr/sbin/lldpcli -f json show chassis":       `{"local-chassis":[{"chassis":[{"id":[{"type":"mac","value":"02:00:00:00:01:01"}],"name":[{"value":"leaf-a"}]}]}]}`,
		"/usr/sbin/lldpctl -f json":                    `{"lldp":[{"interface":[{"name":"swp1"}]}]}`,
		"vtysh -c 'show bgp vrf all summary json'":     `{"default":{}}`,
		"vtysh -c 'show running-config bgpd'":          "frr defaults datacenter\nrouter bgp 4200000000\n!\n",
	}
}

func validRevisionTable() string {
	return validRevisionTableAt("44")
}

func validRevisionTableAt(revision string) string {
	return "Rev ID   State              Apply ID             Apply Date           Type  User     Reason         Message\n" +
		"-------  -----------------  -------------------  -------------------  ----  -------  -------------  ------------------------------------------------------\n" +
		revision + "       applied_and_saved  rev_" + revision + "_apply_1       2026-11-14 16:38:35  CLI   user     Config update  Saved revision\n" +
		"applied  applied            rev_" + revision + "_apply_1       2026-11-14 16:38:35  CLI   user     Config update  Applied revision\n" +
		"empty    inactive\n"
}

func issueFor(snapshot fabric.Snapshot, input string) (fabric.Issue, bool) {
	for _, issue := range snapshot.Issues {
		if issue.Input == input {
			return issue, true
		}
	}
	return fabric.Issue{}, false
}

var _ = Describe("read-only switch collector", func() {
	It("runs only fixed fact commands and sends sudo passwords through stdin", func() {
		target := testSwitchTarget("sudo-secret", false)
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}}
		var opened int
		var progress strings.Builder
		ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
		collector := Collector{openSession: func(_ context.Context, endpoint string, access *config.SSHAccess, timeout time.Duration) (sshprobe.ReadSession, error) {
			opened++
			Expect(endpoint).To(Equal(target.Endpoint))
			Expect(access).To(BeIdenticalTo(target.Access))
			Expect(timeout).To(Equal(3 * time.Second))
			return session, nil
		}}

		snapshot := collector.Collect(ctx, target, 3*time.Second)
		Expect(opened).To(Equal(1))
		Expect(session.closeSeen).To(BeTrue())
		Expect(legacyFactCalls(session)).To(HaveLen(19))
		Expect(snapshot.Configured.Interfaces).To(HaveLen(1))
		Expect(snapshot.Configured.Interfaces[0].Name).To(Equal("swp1"))
		Expect(snapshot.Configured.Interfaces[0].VRF).To(HaveValue(Equal("default")))
		Expect(snapshot.Operational.Interfaces[0].Parent).To(HaveValue(Equal("default")))
		Expect(snapshot.Operational.Interfaces[0].OperState).NotTo(BeNil())
		Expect(snapshot.Operational.LLDP).To(HaveLen(1))
		Expect(snapshot.Operational.BGP).To(HaveLen(1))

		wantCommands := []string{
			"nv config revision",
			"nv show interface --rev applied -o json",
			"nv show vrf --rev applied -o json",
			"nv show router --rev applied -o json",
			"nv show bridge --rev applied -o json",
			"nv show interface -o json",
			"nv show bridge -o json",
			"ip -j address show",
			"ip -j nexthop show",
			"ip -j -4 route show table all",
			"ip -j -6 route show table all",
			"ip -j nexthop show",
			"sudo -S -p '' -- /usr/sbin/lldpctl -f json",
			"sudo -S -p '' -- vtysh -c 'show bgp vrf all summary json'",
			"sudo -S -p '' -- vtysh -c 'show running-config bgpd'",
			"nv show interface swp1 --rev applied -o json",
			"nv show interface swp1 -o json",
			"sudo -S -p '' -- /usr/sbin/lldpcli -f json show chassis",
			"nv config revision",
		}
		for i, call := range legacyFactCalls(session) {
			Expect(call.command).To(Equal(wantCommands[i]))
			Expect(call.command).NotTo(ContainSubstring("sudo-secret"))
			if i <= 11 || i == 15 || i == 16 || i == 18 {
				Expect(call.stdin).To(BeFalse())
				Expect(call.input).To(BeEmpty())
			} else {
				Expect(call.stdin).To(BeTrue())
				Expect(string(call.input)).To(Equal("sudo-secret\n"))
			}
		}
		Expect(progress.String()).NotTo(ContainSubstring("sudo-secret"))
	})

	It("reads kernel nexthops around routes and retains missing initial capability", func() {
		target := testSwitchTarget("", true)
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{"ip -j nexthop show": &sshprobe.Error{Code: "SSH_COMMAND_FAILED", Stage: "switch"}}}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		for _, input := range []string{"operationalNextHopsBefore", "operationalNextHopsAfter"} {
			issue, found := issueFor(snapshot, input)
			Expect(found).To(BeTrue())
			Expect(issue.Code).To(Equal("SSH_COMMAND_FAILED"))
		}
		Expect(snapshot.Operational.IPv4Routes).NotTo(BeNil())
		Expect(*snapshot.Operational.IPv4Routes).To(BeEmpty())
		var order []string
		for _, call := range session.calls {
			if strings.HasPrefix(call.command, "ip -j ") && call.command != "ip -j address show" {
				order = append(order, call.command)
			}
		}
		Expect(order).To(Equal([]string{"ip -j nexthop show", "ip -j -4 route show table all", "ip -j -6 route show table all", "ip -j nexthop show"}))
	})

	It("resolves every stable kernel group member and rejects a changed referenced leaf", func() {
		for _, changed := range []bool{false, true} {
			target := testSwitchTarget("", true)
			outputs := validOutputs()
			outputs["ip -j -4 route show table all"] = `[{"dst":"198.51.100.0/29","nhid":100,"protocol":"bgp","flags":[]}]`
			outputs["ip -j nexthop show"] = `[{"id":100,"group":[{"id":101},{"id":102}]},{"id":101,"dev":"swp1","gateway":"fe80::1"},{"id":102,"dev":"swp2","gateway":"fe80::2"}]`
			session := &fakeSession{outputs: outputs, failures: map[string]error{}}
			reads := 0
			session.onRun = func(_ context.Context, command string) {
				if command == "ip -j nexthop show" {
					reads++
					if changed && reads == 2 {
						outputs[command] = strings.Replace(outputs[command], "fe80::2", "fe80::3", 1)
					}
				}
			}
			collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
				return session, nil
			}}
			snapshot := collector.Collect(context.Background(), target, time.Second)
			Expect(reads).To(Equal(2))
			Expect(snapshot.Operational.IPv4Routes).NotTo(BeNil())
			if changed {
				Expect((*snapshot.Operational.IPv4Routes)[0].NextHops).To(BeEmpty())
			} else {
				Expect((*snapshot.Operational.IPv4Routes)[0].NextHops).To(Equal([]fabric.RouteNextHop{{Interface: "swp1", Gateway: "fe80::1"}, {Interface: "swp2", Gateway: "fe80::2"}}))
			}
		}
	})

	It("uses noninteractive sudo with key authentication", func() {
		target := testSwitchTarget("", true)
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		collector.Collect(context.Background(), target, time.Second)
		Expect(legacyFactCalls(session)).To(HaveLen(19))
		for i := 0; i < 12; i++ {
			Expect(legacyFactCalls(session)[i].command).NotTo(HavePrefix("sudo "))
			Expect(legacyFactCalls(session)[i].stdin).To(BeFalse())
		}
		for _, i := range []int{12, 13, 14, 17} {
			Expect(legacyFactCalls(session)[i].command).To(HavePrefix("sudo -n -- "))
			Expect(legacyFactCalls(session)[i].stdin).To(BeFalse())
		}
	})

	It("does not elevate commands when sudo is disabled", func() {
		target := testSwitchTarget("sudo-secret", false)
		target.Access.Sudo = false
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		collector.Collect(context.Background(), target, time.Second)
		Expect(legacyFactCalls(session)).To(HaveLen(19))
		for _, call := range legacyFactCalls(session) {
			Expect(call.command).NotTo(HavePrefix("sudo "))
			Expect(call.stdin).To(BeFalse())
		}
	})

	It("preserves successful sources when commands fail or return malformed JSON", func() {
		target := testSwitchTarget("sudo-secret", false)
		session := &fakeSession{
			outputs: validOutputs(),
			failures: map[string]error{
				"nv show interface -o json":           &sshprobe.Error{Code: "PERMISSION_DENIED", Stage: "operational-interface"},
				"vtysh -c 'show running-config bgpd'": &sshprobe.Error{Code: "PERMISSION_DENIED", Stage: "frr-running-config"},
			},
		}
		session.outputs["nv show bridge -o json"] = "{malformed"
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(legacyFactCalls(session)).To(HaveLen(19))
		Expect(snapshot.Configured.Interfaces).To(HaveLen(1))
		Expect(snapshot.Operational.Interfaces).To(BeEmpty())
		issue, found := issueFor(snapshot, "operationalInterfaces")
		Expect(found).To(BeTrue())
		Expect(issue.Field).To(Equal("$"))
		Expect(issue.Code).To(Equal("PERMISSION_DENIED"))
		issue, found = issueFor(snapshot, "operationalBridge")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("malformed_json"))
		issue, found = issueFor(snapshot, "frrRunningBGP")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("PERMISSION_DENIED"))
		Expect(snapshot.Configured.FRRPolicy).To(BeNil())
		Expect(fmt.Sprint(snapshot.Issues)).NotTo(ContainSubstring("{malformed"))
	})

	It("retains source-specific revision permission failures", func() {
		target := testSwitchTarget("sudo-secret", false)
		session := &fakeSession{
			outputs: validOutputs(),
			failures: map[string]error{
				"nv config revision": &sshprobe.Error{Code: "PERMISSION_DENIED", Stage: "revision-before"},
			},
		}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(snapshot.Revision).To(BeNil())
		issue, found := issueFor(snapshot, "revisionBefore")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("PERMISSION_DENIED"))
	})

	It("rejects applied or saved revision changes during the read sequence", func() {
		target := testSwitchTarget("sudo-secret", false)
		outputs := validOutputs()
		session := &fakeSession{outputs: outputs, failures: map[string]error{}}
		revisionReads := 0
		session.onRun = func(_ context.Context, command string) {
			if command != "nv config revision" {
				return
			}
			revisionReads++
			if revisionReads == 2 {
				outputs[command] = validRevisionTableAt("45")
			}
		}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(revisionReads).To(Equal(2))
		Expect(snapshot.Revision).To(BeNil())
		issue, found := issueFor(snapshot, "revision")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("revision_changed"))
	})

	It("stops after cancellation and reports remaining observations as canceled", func() {
		target := testSwitchTarget("sudo-secret", false)
		ctx, cancel := context.WithCancel(context.Background())
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}, onRun: func(context.Context, string) { cancel() }}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(ctx, target, time.Second)
		Expect(session.calls).To(HaveLen(1))
		Expect(session.calls[0].command).To(Equal(asicOSCommand))
		Expect(legacyFactCalls(session)).To(BeEmpty())
		Expect(session.closeSeen).To(BeTrue())
		issue, found := issueFor(snapshot, "appliedVRFs")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
		issue, found = issueFor(snapshot, "appliedInterface/swp1")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
	})

	It("rejects malformed or duplicate port names before opening SSH", func() {
		target := testSwitchTarget("sudo-secret", false)
		opened := false
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			opened = true
			return nil, nil
		}}
		for _, ports := range [][]config.SwitchPort{{{Name: "swp1; id"}}, {{Name: "swp1"}, {Name: "swp1"}}} {
			target.Ports = ports
			snapshot := collector.Collect(context.Background(), target, time.Second)
			Expect(opened).To(BeFalse())
			Expect(snapshot.Issues[0].Code).To(Equal("SWITCH_PORTS_INVALID"))
		}
	})

	It("reports connection and cleanup failures without raw errors", func() {
		target := testSwitchTarget("sudo-secret", false)
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return nil, errors.New("secret connection diagnostic")
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(snapshot.Issues).To(HaveLen(1))
		Expect(snapshot.Issues[0].Code).To(Equal("SSH_OBSERVATION_FAILED"))
		Expect(snapshot.Issues[0].Message).NotTo(ContainSubstring("secret"))

		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}, closeErr: errors.New("secret cleanup detail")}
		collector = Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot = collector.Collect(context.Background(), target, time.Second)
		Expect(session.closeSeen).To(BeTrue())
		issue, found := issueFor(snapshot, "sshCleanup")
		Expect(found).To(BeTrue())
		Expect(issue.Message).To(Equal("SSH session cleanup could not be confirmed."))
		Expect(fmt.Sprint(snapshot.Issues)).NotTo(ContainSubstring("secret"))
	})

	It("rejects unsupported platforms and canceled calls before opening SSH", func() {
		target := testSwitchTarget("sudo-secret", false)
		opened := false
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			opened = true
			return nil, nil
		}}
		target.Platform = "sonic"
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(opened).To(BeFalse())
		Expect(snapshot.Issues[0].Code).To(Equal("SWITCH_PLATFORM_UNSUPPORTED"))
		target.Platform = platformCumulusNVUE
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		snapshot = collector.Collect(ctx, target, time.Second)
		Expect(opened).To(BeFalse())
		Expect(snapshot.Issues[0].Code).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
	})

	It("rejects multiline sudo credentials without invoking privileged reads", func() {
		target := testSwitchTarget("line1\nline2", false)
		session := &fakeSession{outputs: validOutputs(), failures: map[string]error{}}
		collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
			return session, nil
		}}
		snapshot := collector.Collect(context.Background(), target, time.Second)
		Expect(legacyFactCalls(session)).To(HaveLen(15))
		issue, found := issueFor(snapshot, "lldp")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("SSH_CONFIGURATION_INVALID"))
		Expect(fmt.Sprint(snapshot.Issues)).NotTo(ContainSubstring("line1"))
	})
})

var _ = It("keeps issue source identifiers aligned with parser inputs", func() {
	parsed := fabric.ParseNVUE(fabric.NVUEInputs{RevisionCollectionRequested: true})
	inputs := map[string]bool{}
	for _, issue := range parsed.Issues {
		inputs[issue.Input] = true
	}
	for _, expected := range []string{"revisionBefore", "revisionAfter", "appliedInterfaces", "appliedVRFs", "appliedRouter", "appliedBridge", "operationalInterfaces", "operationalBridge", "operationalAddresses", "operationalRoutesIPv4", "operationalRoutesIPv6", "lldp", "frrBGPSummary"} {
		Expect(inputs).To(HaveKey(expected))
	}
})
