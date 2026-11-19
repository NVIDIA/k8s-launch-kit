// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/validationreport"
)

//go:embed report.html.tmpl
var reportTemplate embed.FS

// ReportMetadata supplies source-safe display coordinates for the retained run.
type ReportMetadata struct {
	Release     string
	UseCase     string
	Version     string
	GeneratedAt time.Time
}

const maxAttentionRows = 12

type reportBody struct {
	Diagnostic         *Diagnostic
	Hosts              []HostID
	Devices            []DeviceID
	RuntimeBudget      time.Duration
	DisabledRuntime    *RuntimeCoverage
	AttentionRows      []reportRow
	RemainingAttention int
	Groups             []reportGroup
}

type reportRow struct {
	*CheckResult
	Group string
}

type reportGroup struct {
	Title string
	Rows  []reportRow
	reportCounts
}

func (g reportGroup) OpenDetails() bool {
	return g.needsAttention() > 0 && len(g.Rows) <= 20
}

type reportCounts struct {
	Passed, Failed, Pending, Unverified, Skipped int
}

func (c *reportCounts) add(state State) {
	switch state {
	case Passed:
		c.Passed++
	case Failed:
		c.Failed++
	case Pending:
		c.Pending++
	case Skipped:
		c.Skipped++
	default:
		c.Unverified++
	}
}

func (c reportCounts) needsAttention() int {
	return c.Failed + c.Pending + c.Unverified
}

// RenderHTML renders a previously evaluated DPF result.
func RenderHTML(w io.Writer, result Result, metadata ReportMetadata) error {
	tmpl, err := validationreport.NewTemplate(reportTemplate, "report.html.tmpl", template.FuncMap{
		"stateClass": reportStateClass,
		"reasonText": func(reason Reason) string { return strings.ReplaceAll(string(reason), "_", " ") },
	})
	if err != nil {
		return err
	}
	if metadata.GeneratedAt.IsZero() {
		metadata.GeneratedAt = time.Now()
	}
	body := reportBody{
		Diagnostic:      NormalizeDiagnostic(result.Diagnostic),
		Hosts:           result.Scope.Hosts,
		Devices:         result.Scope.Devices,
		RuntimeBudget:   result.RuntimeBudget,
		DisabledRuntime: result.DisabledRuntime,
	}
	outcome := reportOutcome(result)
	groupIndexes := make(map[string]int)
	for i := range result.Checks {
		check := &result.Checks[i]
		title := reportGroupTitle(check.CheckSpec)
		index, exists := groupIndexes[title]
		if !exists {
			index = len(body.Groups)
			groupIndexes[title] = index
			body.Groups = append(body.Groups, reportGroup{Title: title})
		}
		group := &body.Groups[index]
		group.Rows = append(group.Rows, reportRow{CheckResult: check})
		group.add(check.Outcome.State)
		if check.Outcome.State == Passed || check.Outcome.State == Skipped {
			continue
		}
		if len(body.AttentionRows) < maxAttentionRows {
			body.AttentionRows = append(body.AttentionRows, reportRow{CheckResult: check, Group: title})
		} else {
			body.RemainingAttention++
		}
	}
	for _, group := range body.Groups {
		if attention := group.needsAttention(); attention > 0 {
			outcome.Reasons = append(outcome.Reasons, fmt.Sprintf("%s: %d of %d checks need attention", group.Title, attention, len(group.Rows)))
		}
	}
	page := validationreport.Page{
		Title:       "NVIDIA Kubernetes Launch Kit DPF Validation Report",
		Heading:     "DPF Validation Report",
		GeneratedAt: metadata.GeneratedAt,
		Version:     safeText(metadata.Version),
		Outcome:     outcome,
	}
	if metadata.Release != "" {
		page.Metadata = append(page.Metadata, validationreport.Field{Label: "DPF release", Value: safeText(metadata.Release)})
	}
	if metadata.UseCase != "" {
		page.Metadata = append(page.Metadata, validationreport.Field{Label: "Use case", Value: safeText(metadata.UseCase)})
	}
	return tmpl.Execute(w, validationreport.View[reportBody]{Page: page, Body: body})
}

func reportOutcome(result Result) validationreport.OutcomeView {
	var outcome validationreport.OutcomeView
	switch result.Verdict {
	case VerdictPassed:
		outcome.Label = "✓ VALIDATION PASSED"
		outcome.Style = validationreport.OutcomeSuccess
	case VerdictResourcesPassed:
		outcome.Label = "✓ RESOURCE CHECKS PASSED"
		outcome.Style = validationreport.OutcomeSuccess
	case VerdictFailed:
		outcome.Label = "✗ VALIDATION FAILED"
		outcome.Style = validationreport.OutcomeFailure
	default:
		outcome.Label = "VALIDATION INCOMPLETE"
		outcome.Style = validationreport.OutcomeWarning
	}
	switch result.Fatal {
	case FatalInput:
		outcome.Reasons = append(outcome.Reasons, "Validation input or preparation could not complete.")
	case FatalAccess:
		outcome.Reasons = append(outcome.Reasons, "Required infrastructure access was unavailable.")
	case FatalIdentity:
		outcome.Reasons = append(outcome.Reasons, "Selected identities changed or could not be verified.")
	case FatalCanceled:
		outcome.Reasons = append(outcome.Reasons, "Validation was canceled.")
	case FatalTimeout:
		outcome.Reasons = append(outcome.Reasons, "Validation reached its deadline.")
	case FatalObservation:
		outcome.Reasons = append(outcome.Reasons, "Observation could not complete safely.")
	}
	switch result.Mode {
	case ModeResources:
		outcome.Notes = append(outcome.Notes, "Requested resource-only scope covers selected DPF setup and HBN workloads. Physical, host networking, switch, BGP, route, and traffic probes are excluded.")
	case ModeFull:
		outcome.Notes = append(outcome.Notes, "Requested full scope includes selected DPF setup and HBN workloads, physical identities, host and switch networking, BGP, installed routes, and directed source-bound IPv4 ICMP (ordinary and MTU-sized DF).")
	}
	if result.Mode == ModeResources || result.Mode == ModeFull {
		outcome.Notes = append(outcome.Notes, "Coverage excludes complete software cache payload hashing, physically installed bytes, RDMA, throughput, every ECMP member, and failover.")
	}
	return outcome
}

func reportGroupTitle(check CheckSpec) string {
	id := string(check.ID)
	switch {
	case strings.HasPrefix(id, "runtime/bgp/"):
		return "BGP peers"
	case strings.HasPrefix(id, "runtime/route/"):
		return "Installed routes by directed DPU pair"
	case strings.HasPrefix(id, "runtime/traffic/"):
		if strings.HasSuffix(id, "/df") {
			return "MTU-sized DF ICMP by directed host pair"
		}
		return "Ordinary ICMP by directed host pair"
	case strings.HasPrefix(id, "runtime/"):
		return "Runtime coverage"
	}
	switch check.Area {
	case AreaPreparation:
		return "Validation preparation"
	case AreaManagement:
		return "Management cluster"
	case AreaOperator:
		return "DPF Operator"
	case AreaDefinitions:
		return "DPF resource definitions"
	case AreaSystem:
		return "System components and dependencies"
	case AreaTenant:
		return "Tenant cluster"
	case AreaDevices:
		return "DPU enrollment"
	case AreaSoftware:
		return "DPU software"
	case AreaHBN:
		return "HBN resources"
	case AreaWorkloads:
		return "HBN workloads"
	case AreaPhysical:
		return "Physical topology and host networking"
	default:
		return "Other checks"
	}
}

func reportStateClass(state State) string {
	switch state {
	case Passed:
		return "state-success"
	case Failed:
		return "state-error"
	case Pending:
		return "state-inprogress"
	case Skipped:
		return "state-missing"
	default:
		return "state-unknown"
	}
}
