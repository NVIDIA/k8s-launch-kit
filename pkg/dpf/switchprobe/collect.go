// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package switchprobe collects Cumulus facts without network configuration writes.
package switchprobe

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const platformCumulusNVUE = "cumulus-nvue"

var physicalPortName = regexp.MustCompile(`^swp[0-9]+(s[0-9]+)?$`)

type sessionOpener func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ReadSession, error)

// Collector reads a fixed set of Cumulus facts using the configured endpoint
// and trust snapshot. The qualified SDK diagnostic temporarily adjusts router
// log verbosity. It never configures forwarding or follows discovered
// addresses. openSession is injectable for focused lifecycle tests.
type Collector struct {
	openSession sessionOpener
}

type commandSpec struct {
	input      string
	command    string
	privileged bool
	set        func(*fabric.NVUEInputs, []byte)
}

func commands(ports []config.SwitchPort) []commandSpec {
	result := []commandSpec{
		{input: "revisionBefore", command: "nv config revision", set: func(in *fabric.NVUEInputs, out []byte) { in.RevisionBefore = out }},
		{input: "appliedInterfaces", command: "nv show interface --rev applied -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.AppliedInterfaces = out }},
		{input: "appliedVRFs", command: "nv show vrf --rev applied -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.AppliedVRFs = out }},
		{input: "appliedRouter", command: "nv show router --rev applied -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.AppliedRouter = out }},
		{input: "appliedBridge", command: "nv show bridge --rev applied -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.AppliedBridge = out }},
		{input: "operationalInterfaces", command: "nv show interface -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.OperationalInterfaces = out }},
		{input: "operationalBridge", command: "nv show bridge -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.OperationalBridge = out }},
		{input: "operationalAddresses", command: "ip -j address show", set: func(in *fabric.NVUEInputs, out []byte) { in.OperationalAddresses = out }},
		{input: "operationalRoutesIPv4", command: "ip -j -4 route show table all", set: func(in *fabric.NVUEInputs, out []byte) { in.OperationalRoutesIPv4 = out }},
		{input: "operationalRoutesIPv6", command: "ip -j -6 route show table all", set: func(in *fabric.NVUEInputs, out []byte) { in.OperationalRoutesIPv6 = out }},
		{input: "lldp", command: "/usr/sbin/lldpctl -f json", privileged: true, set: func(in *fabric.NVUEInputs, out []byte) { in.LLDP = out }},
		{input: "frrBGPSummary", command: "vtysh -c 'show bgp vrf all summary json'", privileged: true, set: func(in *fabric.NVUEInputs, out []byte) { in.FRRBGPSummary = out }},
		{input: "frrRunningBGP", command: "vtysh -c 'show running-config bgpd'", privileged: true, set: func(in *fabric.NVUEInputs, out []byte) { in.FRRRunningBGP = out }},
	}
	for _, port := range ports {
		name := port.Name
		result = append(result,
			commandSpec{input: "appliedInterface/" + name, command: "nv show interface " + name + " --rev applied -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.SelectedAppliedInterfaces[name] = out }},
			commandSpec{input: "operationalInterface/" + name, command: "nv show interface " + name + " -o json", set: func(in *fabric.NVUEInputs, out []byte) { in.SelectedOperationalInterfaces[name] = out }},
		)
	}
	result = append(result, commandSpec{input: "localLLDP", command: "/usr/sbin/lldpcli -f json show chassis", privileged: true, set: func(in *fabric.NVUEInputs, out []byte) { in.LocalLLDP = out }})
	result = append(result, commandSpec{input: "revisionAfter", command: "nv config revision", set: func(in *fabric.NVUEInputs, out []byte) { in.RevisionAfter = out }})
	return result
}

type observationFailure struct {
	code    string
	message string
}

// Collect reads configured and operational facts from one explicitly selected
// switch. Command failures remain source-specific issues; successful sources
// are still parsed and returned. A canceled context stops all later commands.
func (c Collector) Collect(ctx context.Context, target config.SwitchTarget, timeout time.Duration) (snapshot fabric.Snapshot) {
	started := time.Now()
	if ctx == nil {
		return connectionIssue("SSH_CONFIGURATION_INVALID", "A discovery context is required.")
	}
	logger := log.FromContext(ctx).WithName("dpf-switch-discovery").WithValues("platform", target.Platform)
	ctx = log.IntoContext(ctx, logger)
	output := ui.FromContext(ctx)
	if target.Platform != platformCumulusNVUE {
		return connectionIssue("SWITCH_PLATFORM_UNSUPPORTED", "Only the cumulus-nvue switch platform is supported.")
	}
	if target.Access == nil || timeout <= 0 {
		return connectionIssue("SSH_CONFIGURATION_INVALID", "Switch discovery requires explicit SSH access and a positive timeout.")
	}
	if len(target.Ports) > 256 {
		return connectionIssue("SWITCH_PORTS_INVALID", "At most 256 selected switch ports are supported.")
	}
	seenPorts := map[string]bool{}
	for _, port := range target.Ports {
		if !physicalPortName.MatchString(port.Name) || seenPorts[port.Name] {
			return connectionIssue("SWITCH_PORTS_INVALID", "Selected ports must have unique physical swp port names.")
		}
		seenPorts[port.Name] = true
	}
	if err := ctx.Err(); err != nil {
		return connectionIssue("SSH_TIMEOUT_OR_CANCELLED", sshprobe.Reason(&sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED"}))
	}
	label := target.Name
	if label == "" {
		label = "configured switch"
	}
	output.Info("[RUNNING] Switch %s: authenticated SSH session", label)
	opener := c.openSession
	if opener == nil {
		opener = sshprobe.OpenReadSession
	}
	session, err := opener(ctx, target.Endpoint, target.Access, timeout)
	if err != nil {
		output.Info("[FAIL] Switch %s: SSH session (%s)", label, sshprobe.Code(err))
		return connectionIssue(sshprobe.Code(err), sshprobe.Reason(err))
	}
	output.Info("[PASS] Switch %s: authenticated SSH session established", label)

	output.Info("[RUNNING] Switch %s: qualified ASIC source identities", label)
	asic, asicFailure := beginASIC(ctx, session, target.Access)
	inputs := fabric.NVUEInputs{RevisionCollectionRequested: true, SelectedAppliedInterfaces: map[string][]byte{}, SelectedOperationalInterfaces: map[string][]byte{}}
	for _, port := range target.Ports {
		inputs.SelectedAppliedInterfaces[port.Name] = nil
		inputs.SelectedOperationalInterfaces[port.Name] = nil
	}
	failures := map[string]observationFailure{}
	commandList := commands(target.Ports)
	completed := 0
	defer func() {
		output.Info("[RUNNING] Switch %s: SSH session cleanup", label)
		closeErr := session.Close()
		if closeErr != nil {
			output.Info("[UNAVAILABLE] Switch %s: SSH cleanup could not be confirmed", label)
			snapshot.Issues = append(snapshot.Issues, fabric.Issue{Input: "sshCleanup", Field: "$", Code: sshprobe.Code(closeErr), Message: "SSH session cleanup could not be confirmed."})
		} else {
			output.Info("[PASS] Switch %s: SSH session cleanup completed", label)
		}
		logger.V(1).Info("Switch collection completed", "commandsAttempted", completed, "issues", len(snapshot.Issues), "duration", time.Since(started))
	}()

	for i, spec := range commandList {
		if err := ctx.Err(); err != nil {
			for _, pending := range commandList[i:] {
				failures[pending.input] = observationFailure{code: "SSH_TIMEOUT_OR_CANCELLED", message: sshprobe.Reason(&sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED"})}
			}
			break
		}
		command, stdin, err := commandFor(target.Access, spec)
		if err != nil {
			message := sshprobe.Reason(err)
			if diagnostic, ok := err.(*sshprobe.Error); ok && diagnostic.Stage == "sudo-input" {
				message = "Configured sudo password must be a single line."
			}
			failures[spec.input] = observationFailure{code: sshprobe.Code(err), message: message}
			continue
		}
		output.Info("[RUNNING] Switch %s: %s", label, spec.input)
		stage := "switch"
		var text string
		if len(stdin) > 0 {
			text, err = session.RunWithInput(ctx, stage, command, stdin)
			clear(stdin)
		} else {
			text, err = session.Run(ctx, stage, command)
		}
		completed++
		if err != nil {
			failures[spec.input] = observationFailure{code: sshprobe.Code(err), message: sshprobe.Reason(err)}
			output.Info("[UNAVAILABLE] Switch %s: %s (%s)", label, spec.input, sshprobe.Code(err))
			if ctx.Err() != nil {
				for _, pending := range commandList[i+1:] {
					failures[pending.input] = observationFailure{code: "SSH_TIMEOUT_OR_CANCELLED", message: sshprobe.Reason(&sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED"})}
				}
				break
			}
			continue
		}
		spec.set(&inputs, []byte(text))
		output.Info("[PASS] Switch %s: %s", label, spec.input)
	}

	snapshot = fabric.ParseNVUE(inputs)
	if asic != nil && ctx.Err() == nil {
		output.Info("[RUNNING] Switch %s: direct SDK ASIC RIF/VRF observation", label)
		snapshot.ASIC, asicFailure = asic.finish(ctx, target.Ports)
	}
	if asicFailure != nil {
		output.Info("[UNAVAILABLE] Switch %s: ASIC observation (%s)", label, asicFailure.Code)
		snapshot.Issues = append(snapshot.Issues, *asicFailure)
	}
	for i := range snapshot.Issues {
		issue := &snapshot.Issues[i]
		failure, failed := failures[issue.Input]
		if failed && issue.Field == "$" && issue.Code == "missing_output" {
			issue.Code = failure.code
			issue.Message = failure.message
		}
	}
	for input, failure := range failures {
		found := false
		for _, issue := range snapshot.Issues {
			if issue.Input == input {
				found = true
				break
			}
		}
		if !found {
			snapshot.Issues = append(snapshot.Issues, fabric.Issue{Input: input, Field: "$", Code: failure.code, Message: failure.message})
		}
	}
	return snapshot
}

func commandFor(access *config.SSHAccess, spec commandSpec) (string, []byte, error) {
	if !spec.privileged || !access.Sudo {
		return spec.command, nil, nil
	}
	_, password := access.Credentials()
	if password == "" {
		return "sudo -n -- " + spec.command, nil, nil
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return "", nil, &sshprobe.Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "sudo-input"}
	}
	stdin := make([]byte, len(password)+1)
	copy(stdin, password)
	stdin[len(stdin)-1] = '\n'
	return "sudo -S -p '' -- " + spec.command, stdin, nil
}

func connectionIssue(code, message string) fabric.Snapshot {
	if strings.TrimSpace(code) == "" {
		code = "SSH_OBSERVATION_FAILED"
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("Switch observation failed (%s).", code)
	}
	return fabric.Snapshot{Issues: []fabric.Issue{{Input: "connection", Field: "$", Code: code, Message: message}}}
}
