// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package switchprobe

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func qualifiedASICOutputs() map[string]string {
	out := validOutputs()
	read := func(name string) string {
		raw, err := os.ReadFile("../fabric/testdata/asic/" + name)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return string(raw)
	}
	out[asicOSCommand] = "ID=cumulus-linux\nVERSION_ID=5.15.1\nCODE_DROP=\"" + qualifiedCodeDrop + "\"\n"
	out[asicHashCommand] = qualifiedRIFHash + "  /usr/bin/sx_api_router_interface_dump_all.py\n" + qualifiedHelperHash + "  /usr/bin/flex_acl_common.py\n"
	out[asicConfigCommand] = read("config.json")
	out[asicPendingCommand] = "" // Actual qualified empty diff: successful exit, zero stdout bytes.
	out[asicLinuxCommand] = read("linux.json")
	out[asicRootCommand] = read("root.txt")
	out["cat /cumulus/switchd/run/software-tables/11"] = read("interfaces.txt")
	out["cat /cumulus/switchd/run/software-tables/30"] = read("vrfs.txt")
	out[asicRIFCommand] = read("rifs.txt")
	return out
}

func collectASICFixture(ctx context.Context, session *fakeSession) fabric.Snapshot {
	target := testSwitchTarget("", true)
	target.Ports = []config.SwitchPort{{Name: "swp1s0"}, {Name: "swp1s1"}, {Name: "swp19s0"}, {Name: "swp19s1"}}
	collector := Collector{openSession: func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error) {
		return session, nil
	}}
	return collector.Collect(ctx, target, 3*time.Second)
}

func stockCalls(session *fakeSession) int {
	result := 0
	for _, call := range session.calls {
		if wrappedCommand(call.command) == asicRIFCommand {
			result++
		}
	}
	return result
}

// Existing transport tests inspect the original NVUE/LLDP/FRR command set.
// Their unqualified fixtures now also perform exactly one OS capability read.
func legacyFactCalls(session *fakeSession) []commandCall {
	result := make([]commandCall, 0, len(session.calls))
	for _, call := range session.calls {
		if wrappedCommand(call.command) != asicOSCommand {
			result = append(result, call)
		}
	}
	return result
}

var _ = Describe("qualified stock SDK diagnostic collection", func() {
	It("qualifies exact build/tool identities before one SDK read and fences all normal fact collection", func() {
		session := &fakeSession{outputs: qualifiedASICOutputs(), failures: map[string]error{}}
		got := collectASICFixture(context.Background(), session)
		Expect(got.ASIC).NotTo(BeNil())
		Expect(got.ASIC.Ports).To(HaveLen(4))
		Expect(stockCalls(session)).To(Equal(1))
		Expect(session.closeSeen).To(BeTrue())
		_, hasIssue := issueFor(got, "asic")
		Expect(hasIssue).To(BeFalse())
		Expect(session.calls[0].command).To(Equal(asicOSCommand))
		Expect(session.calls[1].command).To(Equal(asicHashCommand))
		beforeConfig, firstNVUE, sdk, afterConfig := -1, -1, -1, -1
		for index, call := range session.calls {
			command := wrappedCommand(call.command)
			switch command {
			case asicConfigCommand:
				if beforeConfig < 0 {
					beforeConfig = index
				} else {
					afterConfig = index
				}
			case "nv config revision":
				if firstNVUE < 0 {
					firstNVUE = index
				}
			case asicRIFCommand:
				sdk = index
			}
			Expect(command).NotTo(ContainSubstring("python -"))
			Expect(command).NotTo(ContainSubstring("python3 -"))
			Expect(command).NotTo(ContainSubstring("nv set"))
			Expect(command).NotTo(ContainSubstring("nv config apply"))
		}
		Expect(beforeConfig).To(BeNumerically("<", firstNVUE))
		Expect(sdk).To(BeNumerically(">", firstNVUE))
		Expect(afterConfig).To(BeNumerically(">", sdk))
	})
	It("keeps unsupported builds, changed tools and unreadable mappings unmeasured without SDK invocation", func() {
		for _, test := range []struct {
			name, command, value string
			failure              error
		}{
			{name: "unsupported build", command: asicOSCommand, value: "ID=cumulus-linux\nVERSION_ID=5.16.0\n"},
			{name: "changed script hash", command: asicHashCommand, value: "unreviewed"},
			{name: "permission denied", command: "cat /cumulus/switchd/run/software-tables/11", failure: &sshprobe.Error{Code: "SSH_COMMAND_FAILED", Stage: "switch-asic"}},
			{name: "traversal table ID", command: asicRootCommand, value: strings.ReplaceAll(qualifiedASICOutputs()[asicRootCommand], "00000011", "../../11")},
			{name: "pending read denied", command: asicPendingCommand, failure: &sshprobe.Error{Code: "SSH_COMMAND_FAILED", Stage: "switch-asic"}},

			{name: "pending configuration", command: asicPendingCommand, value: `{"set":{"x":1}}`},
			{name: "malformed Linux identities", command: asicLinuxCommand, value: `{}`},
		} {
			By(test.name)
			session := &fakeSession{outputs: qualifiedASICOutputs(), failures: map[string]error{}}
			if test.failure != nil {
				session.failures[test.command] = test.failure
			} else {
				session.outputs[test.command] = test.value
			}
			got := collectASICFixture(context.Background(), session)
			Expect(got.ASIC).To(BeNil())
			Expect(stockCalls(session)).To(BeZero())
			_, found := issueFor(got, "asic")
			Expect(found).To(BeTrue())
			for _, call := range session.calls {
				Expect(call.command).NotTo(ContainSubstring("/../../"))
			}
		}
	})
	It("rechecks executable identity immediately before the diagnostic", func() {
		session := &fakeSession{outputs: qualifiedASICOutputs(), failures: map[string]error{}}
		session.onRun = func(_ context.Context, command string) {
			if wrappedCommand(command) == "nv config revision" {
				session.outputs[asicHashCommand] = "replaced during fact collection"
			}
		}
		got := collectASICFixture(context.Background(), session)
		Expect(got.ASIC).To(BeNil())
		Expect(stockCalls(session)).To(BeZero())
		issue, found := issueFor(got, "asic")
		Expect(found).To(BeTrue())
		Expect(issue.Code).To(Equal("ASIC_TOOL_UNSUPPORTED"))
	})

	It("stops immediately without retry after SDK failure, restore failure, zero-exit error or cancellation", func() {
		for _, kind := range []string{"command error", "restore error", "zero exit uninitialized", "cancel"} {
			By(kind)
			session := &fakeSession{outputs: qualifiedASICOutputs(), failures: map[string]error{}}
			ctx, cancel := context.WithCancel(context.Background())
			switch kind {
			case "command error":
				session.failures[asicRIFCommand] = &sshprobe.Error{Code: "SSH_COMMAND_FAILED", Stage: "switch-asic"}
			case "restore error":
				session.outputs[asicRIFCommand] = "Failed to set router API verbosity level\n"
			case "zero exit uninitialized":
				session.outputs[asicRIFCommand] = "The router module is not initialized. Dump is not available.\n"
			case "cancel":
				session.onRun = func(_ context.Context, command string) {
					if wrappedCommand(command) == asicRIFCommand {
						cancel()
					}
				}
			}
			got := collectASICFixture(ctx, session)
			cancel()
			Expect(got.ASIC).To(BeNil())
			Expect(stockCalls(session)).To(Equal(1))
			Expect(wrappedCommand(session.calls[len(session.calls)-1].command)).To(Equal(asicRIFCommand))
			Expect(session.closeSeen).To(BeTrue())
			issue, found := issueFor(got, "asic")
			Expect(found).To(BeTrue())
			Expect(issue.Message).To(ContainSubstring("without retry"))
		}
	})
	It("invalidates stable-looking software snapshots when config, identities or tool build changes during collection", func() {
		for _, command := range []string{asicConfigCommand, asicLinuxCommand, asicHashCommand} {
			session := &fakeSession{outputs: qualifiedASICOutputs(), failures: map[string]error{}}
			session.onRun = func(_ context.Context, called string) {
				if wrappedCommand(called) == asicRIFCommand {
					switch command {
					case asicConfigCommand:
						session.outputs[command] = strings.Replace(session.outputs[command], "switch-a", "switch-b", 1)
					case asicLinuxCommand:
						session.outputs[command] = strings.ReplaceAll(session.outputs[command], "1006", "9999")
					case asicHashCommand:
						session.outputs[command] = "changed"
					}
				}
			}
			got := collectASICFixture(context.Background(), session)
			Expect(got.ASIC).To(BeNil())
			Expect(stockCalls(session)).To(Equal(1))
			_, found := issueFor(got, "asic")
			Expect(found).To(BeTrue())
		}
	})
})
