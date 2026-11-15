// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// textReport presents the same decisions as the structured report. Internal
// record/evidence keys remain in JSON; they are never operator-facing names.
func textReport(w io.Writer, report inventory.Report, input config.Input) error {
	var out strings.Builder
	status := "incomplete"
	switch {
	case report.Execution == "cancelled":
		status = "cancelled"
	case report.Execution == "failed" || report.Discovery.Outcome == "invalid":
		status = "failed"
	case discovery.ExitCode(report) == 0:
		status = "completed"
	}
	fmt.Fprintf(&out, "DPF discovery %s", status)
	if report.Discovery.Mode != "" {
		fmt.Fprintf(&out, " (%s)", displayText(report.Discovery.Mode))
	}
	fmt.Fprintln(&out)
	labels := reportLabels(input, report.Inventory)
	if report.Readiness != nil {
		fmt.Fprintf(&out, "HBN-BGP readiness: %s\n", displayText(report.Readiness.State))
		var blockers []inventory.Check
		for _, check := range report.Readiness.Checks {
			if check.State != "passed" {
				blockers = append(blockers, check)
			}
		}
		if len(blockers) > 0 {
			fmt.Fprintln(&out, "\nRequired before HBN-BGP deployment:")
			writeChecks(&out, blockers, labels)
		}
	}
	passed, required := 0, 0
	for _, check := range report.Discovery.Checks {
		if check.RequiredPhase == "discover" {
			required++
			if check.State == "passed" {
				passed++
			}
		}
	}
	if required > 0 {
		fmt.Fprintf(&out, "Required discovery checks: %d/%d passed\n", passed, required)
	}
	if inv := report.Inventory; inv != nil {
		fmt.Fprintf(&out, "Identified: %d/%d hosts, %d/%d DPUs; host-to-DPU mappings: %d\n", observedCount(inv.Hosts), len(inv.Hosts), observedCount(inv.DPUs), len(inv.DPUs), len(inv.Associations))
		if inv.Software != nil {
			fmt.Fprintf(&out, "Software comparison catalog: DPF %s\n", displayText(inv.Software.Version))
		}
		writeDevices(&out, inv, report.Discovery.Checks, labels)
	}
	var blockers, later, deferred []inventory.Check
	for _, check := range report.Discovery.Checks {
		if check.State == "passed" {
			continue
		}
		switch {
		case check.RequiredPhase == "discover":
			blockers = append(blockers, check)
		case check.State == "not-requested" || check.State == "not-evaluated":
			deferred = append(deferred, check)
		default:
			later = append(later, check)
		}
	}
	if len(blockers)+len(report.Errors) > 0 || report.Discovery.Cleanup.State == "incomplete" {
		fmt.Fprintln(&out, "\nProblems preventing successful discovery:")
		writeChecks(&out, blockers, labels)
		for _, problem := range report.Errors {
			fmt.Fprintf(&out, "  - %s: %s", subjectLabel(labels, problem.InputRef), displayText(problem.Message))
			if problem.Code != "" {
				fmt.Fprintf(&out, " [%s]", displayText(problem.Code))
			}
			if problem.Stage != "" {
				fmt.Fprintf(&out, " (stage: %s)", displayText(problem.Stage))
			}
			if problem.HTTPStatus != 0 {
				fmt.Fprintf(&out, " (HTTP %d)", problem.HTTPStatus)
			}
			fmt.Fprintln(&out)
		}
		if report.Discovery.Cleanup.State == "incomplete" {
			fmt.Fprintln(&out, "  - Temporary resource/session cleanup is incomplete. Inspect the detailed report before retrying.")
		}
	}
	if len(later) > 0 {
		fmt.Fprintln(&out, "\nBefore provisioning (these do not fail inventory discovery):")
		writeChecks(&out, later, labels)
	}
	if len(deferred) > 0 {
		fmt.Fprintln(&out, "\nNot checked in this run:")
		writeChecks(&out, deferred, labels)
	}
	if report.Inventory != nil {
		fmt.Fprintf(&out, "\nCleanup: %s\n", displayText(report.Discovery.Cleanup.State))
	}
	if len(report.Artifacts) > 0 {
		fmt.Fprintln(&out, "\nSaved files:")
		for _, artifact := range report.Artifacts {
			name := map[string]string{"inventory": "Inventory", "partial": "Partial inventory", "report": "Detailed report", "source-backup": "Previous configuration"}[artifact.Kind]
			if name == "" {
				name = displayText(artifact.Kind)
			}
			fmt.Fprintf(&out, "  %s: %s\n", name, displayText(artifact.Path))
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func observedCount(devices []inventory.Device) int {
	count := 0
	for _, device := range devices {
		if device.ObservationState == "observed" && device.ID != "" {
			count++
		}
	}
	return count
}

func reportLabels(input config.Input, inv *inventory.Inventory) map[string]string {
	labels := map[string]string{"management": "Management cluster", "/management": "Management cluster", "inventory": "Inventory", "": "Discovery"}
	nameCounts := map[string]int{}
	for _, endpoint := range append(slices.Clone(input.Site.Discovery.Hosts), input.Site.Discovery.DPUs...) {
		if endpoint.Name != nil {
			nameCounts[displayText(*endpoint.Name)]++
		}
	}
	for _, group := range []struct {
		role      string
		endpoints []config.Endpoint
	}{{"hosts", input.Site.Discovery.Hosts}, {"dpus", input.Site.Discovery.DPUs}} {
		for i, endpoint := range group.endpoints {
			label := endpoint.BMCEndpoint
			if endpoint.SSH != nil {
				label = endpoint.SSH.Address
			}
			if endpoint.Name != nil && *endpoint.Name != "" {
				if nameCounts[displayText(*endpoint.Name)] == 1 {
					label = *endpoint.Name
				} else {
					label = *endpoint.Name + " (" + label + ")"
				}
			}
			labels[fmt.Sprintf("/discovery/%s/%d", group.role, i)] = displayText(label)
		}
	}
	if inv != nil {
		for _, device := range append(slices.Clone(inv.Hosts), inv.DPUs...) {
			label := labels[device.InputRef]
			if device.ReaderAdapter == "host-card-v1" && device.SerialNumber != "" {
				label = "DPU " + device.SerialNumber + " (via " + subjectLabel(labels, device.InputRef) + ")"
			}
			if label == "" {
				switch {
				case device.BMCEndpoint != "":
					label = device.BMCEndpoint
				case device.SSHEndpoint != "":
					label = device.SSHEndpoint
				case device.SerialNumber != "":
					label = "DPU " + device.SerialNumber
				default:
					label = "Unidentified source"
				}
			}
			for _, key := range []string{device.RecordKey, device.ID, device.InputRef} {
				if key == device.InputRef && device.ReaderAdapter == "host-card-v1" {
					continue
				}
				if key != "" {
					labels[key] = displayText(label)
				}
			}
		}
	}
	return labels
}

func subjectLabel(labels map[string]string, subject string) string {
	if label, ok := labels[subject]; ok {
		return label
	}
	return "Unidentified source"
}

func writeDevices(out io.Writer, inv *inventory.Inventory, checks []inventory.Check, labels map[string]string) {
	if len(inv.DPUs) == 0 {
		return
	}
	hosts := map[string]string{}
	for _, association := range inv.Associations {
		hosts[association.DPUID] = subjectLabel(labels, association.HostID)
	}
	admin := map[string]bool{}
	for _, check := range checks {
		if check.ID == "dpf.management.bmc-administrator" {
			admin[check.Subject] = check.State == "passed"
		}
	}
	fmt.Fprintln(out)
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "DPU\tMODEL\tSERIAL\tHOST\tBMC ADMIN")
	devices := slices.Clone(inv.DPUs)
	slices.SortStableFunc(devices, func(a, b inventory.Device) int {
		return strings.Compare(subjectLabel(labels, a.RecordKey), subjectLabel(labels, b.RecordKey))
	})
	for _, device := range devices {
		host := hosts[device.ID]
		if host == "" {
			host = "not established"
		}
		administrator := "not verified"
		if admin[device.RecordKey] {
			administrator = "verified"
		}
		model, serial := device.Model, device.SerialNumber
		if model == "" {
			model = "unknown"
		}
		if serial == "" {
			serial = "unknown"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", subjectLabel(labels, device.RecordKey), displayText(model), displayText(serial), host, administrator)
	}
	_ = table.Flush() // The destination is the report's strings.Builder.
}

func writeChecks(out io.Writer, checks []inventory.Check, labels map[string]string) {
	// Coalesce identical findings while retaining all affected sources. Different
	// messages (for example, different measured versions) remain separate.
	type group struct {
		check    inventory.Check
		subjects []string
	}
	groups := []group{}
	index := map[[4]string]int{}
	for _, check := range checks {
		key := [4]string{check.ID, check.State, check.ReasonCode, check.Message}
		i, found := index[key]
		if !found {
			i = len(groups)
			index[key] = i
			groups = append(groups, group{check: check})
		}
		if !slices.Contains(groups[i].subjects, check.Subject) {
			groups[i].subjects = append(groups[i].subjects, check.Subject)
		}
	}
	for _, g := range groups {
		for i, subject := range g.subjects {
			g.subjects[i] = subjectLabel(labels, subject)
		}
		slices.Sort(g.subjects)
		message := g.check.Message
		switch g.check.ReasonCode {
		case "QUALIFIED_ADAPTER_REQUIRED":
			message = "Hardware/firmware qualification is not established in this build; this is not a detected hardware fault."
		case "BOM_MODEL_UNSUPPORTED":
			message = "Software compatibility is unknown: the selected discovery BOM has no matching model entry."
		case "INVENTORY_MODE":
			message = "Management-network connectivity probes were not requested in inventory mode."
		case "DPF_AUTHENTICATION_NOT_EVALUATED":
			message = "DPF controller authentication is checked during provisioning, not inventory discovery."
		}
		if message == "" {
			message = "No passing result was established; inspect the detailed report."
		}
		fmt.Fprintf(out, "  - %s: %s\n", strings.Join(g.subjects, ", "), displayText(message))
	}
}

func displayText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
