// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package switchprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

const (
	asicOSCommand      = "cat /etc/os-release"
	asicHashCommand    = "sha256sum -- /usr/bin/sx_api_router_interface_dump_all.py /usr/bin/flex_acl_common.py"
	asicRIFCommand     = "/usr/bin/sx_api_router_interface_dump_all.py"
	asicRootCommand    = "cat /cumulus/switchd/run/software-tables/root"
	asicLinuxCommand   = "ip -j -d link show"
	asicConfigCommand  = "nv config show --rev applied -o json"
	asicPendingCommand = "nv config diff --expand -o json"
)

type asicCollection struct {
	session sshprobe.ReadSession
	access  *config.SSHAccess
	inputs  fabric.ASICInputs
	tables  fabric.ASICBackendTables
}

func asicIssue(code, message string) *fabric.Issue {
	return &fabric.Issue{Input: "asic", Field: "$", Code: code, Message: message}
}

// run never retries. In particular, any error from the stock diagnostic stops
// collection: its temporary SDK logging change has no exception-safe restore.
func (c *asicCollection) run(ctx context.Context, command string, privileged bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command, stdin, err := commandFor(c.access, commandSpec{command: command, privileged: privileged})
	if err != nil {
		return nil, err
	}
	var result string
	if len(stdin) > 0 {
		result, err = c.session.RunWithInput(ctx, "switch-asic", command, stdin)
		clear(stdin)
	} else {
		result, err = c.session.Run(ctx, "switch-asic", command)
	}
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}

func (c *asicCollection) qualify(ctx context.Context) *fabric.Issue {
	osRelease, err := c.run(ctx, asicOSCommand, false)
	if err != nil {
		return asicIssue(sshprobe.Code(err), "ASIC build identity could not be read.")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(osRelease)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		_, duplicate := fields[key]
		if !ok || duplicate {
			return asicIssue("ASIC_BUILD_UNSUPPORTED", "ASIC collection requires the qualified Cumulus/SDK build.")
		}
		fields[key] = strings.Trim(value, "\"")
	}
	profile := catalog.CumulusASICReader()
	if fields["ID"] != profile.OSID || fields["VERSION_ID"] != profile.Version || fields["CODE_DROP"] != profile.CodeDrop {
		return asicIssue("ASIC_BUILD_UNSUPPORTED", fmt.Sprintf("ASIC collection requires catalog-qualified Cumulus %s (%s).", profile.Version, profile.CodeDrop))
	}
	hashes, err := c.run(ctx, asicHashCommand, false)
	if err != nil {
		return asicIssue(sshprobe.Code(err), "ASIC tool identity could not be read.")
	}
	want := profile.RIFSHA256 + "  /usr/bin/sx_api_router_interface_dump_all.py\n" + profile.HelperSHA256 + "  /usr/bin/flex_acl_common.py"
	if strings.TrimSpace(string(hashes)) != want {
		return asicIssue("ASIC_TOOL_UNSUPPORTED", "Installed SDK RIF diagnostic or verbosity helper differs from the reviewed version.")
	}
	return nil
}

func (c *asicCollection) capture(ctx context.Context, after bool) *fabric.Issue {
	if issue := c.qualify(ctx); issue != nil {
		return issue
	}
	var root, linux, configBytes, pending, interfaces, vrfs []byte
	for _, step := range []struct {
		command    string
		privileged bool
		out        *[]byte
	}{
		{asicConfigCommand, false, &configBytes}, {asicPendingCommand, false, &pending}, {asicLinuxCommand, false, &linux}, {asicRootCommand, true, &root},
	} {
		value, err := c.run(ctx, step.command, step.privileged)
		if err != nil {
			return asicIssue(sshprobe.Code(err), "ASIC source identity or configuration fence could not be collected.")
		}
		*step.out = value
	}
	tables, err := fabric.ParseASICBackendTables(root)
	if err != nil {
		return asicIssue("ASIC_SOURCE_UNAVAILABLE", err.Error())
	}
	if after && tables != c.tables {
		return asicIssue("ASIC_SNAPSHOT_CHANGED", "Switchd table identity changed during collection.")
	}
	for _, step := range []struct {
		id  uint32
		out *[]byte
	}{{tables.Interfaces, &interfaces}, {tables.VRFs, &vrfs}} {
		// IDs are parsed unsigned numbers, not shell/path text from the response.
		value, err := c.run(ctx, fmt.Sprintf("cat /cumulus/switchd/run/software-tables/%d", step.id), true)
		if err != nil {
			return asicIssue(sshprobe.Code(err), "ASIC backend identity mapping could not be read.")
		}
		*step.out = value
	}
	frame := fabric.ASICIdentityInputs{Root: root, Interfaces: interfaces, VRFs: vrfs, Linux: linux, Config: configBytes, Pending: pending}
	if after {
		c.inputs.After = frame
	} else {
		c.tables = tables
		c.inputs.Before = frame
	}

	return nil
}

func beginASIC(ctx context.Context, session sshprobe.ReadSession, access *config.SSHAccess) (*asicCollection, *fabric.Issue) {
	c := &asicCollection{session: session, access: access}
	if issue := c.capture(ctx, false); issue != nil {
		return nil, issue
	}
	return c, nil
}

func (c *asicCollection) finish(ctx context.Context, ports []config.SwitchPort) (*fabric.ASICObservation, *fabric.Issue) {
	// Preflight parse of every software source must succeed before the diagnostic
	// changes SDK verbosity, even briefly. The later full join still fences them.
	names := make([]string, 0, len(ports))
	for _, port := range ports {
		names = append(names, port.Name)
	}
	if err := fabric.ValidateASICIdentity(c.inputs.Before, names); err != nil {
		return nil, asicIssue("ASIC_SOURCE_UNAVAILABLE", err.Error())
	}
	// Recheck immediately before execution as regular fact collection can take
	// time; a package replacement must not authorize an unreviewed diagnostic.
	if issue := c.qualify(ctx); issue != nil {
		return nil, issue
	}
	raw, err := c.run(ctx, asicRIFCommand, true)
	if err != nil {
		return nil, asicIssue(sshprobe.Code(err), "SDK RIF diagnostic did not complete successfully; SDK log restoration is unconfirmed. Collection stopped without retry or network repair.")
	}
	c.inputs.RIFs, err = fabric.ParseASICRIFDump(raw)
	if err != nil {
		return nil, asicIssue("ASIC_DIAGNOSTIC_FAILED", "SDK RIF output is invalid; SDK log restoration is unconfirmed. Collection stopped without retry or network repair.")
	}
	if issue := c.capture(ctx, true); issue != nil {
		return nil, issue
	}
	result, err := fabric.ParseASICObservation(c.inputs, names)
	if err != nil {
		return nil, asicIssue("ASIC_SOURCE_UNAVAILABLE", err.Error())
	}
	return result, nil
}
