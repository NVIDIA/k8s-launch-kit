// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

// This exercises decorator/Runner policy using the existing component session.
// Native full-observer Run coverage is in run_native_runtime_test.go.
func TestRunAcceptanceClosingReceiptRevokesRunnerCohort(t *testing.T) {
	for _, failure := range []string{"receipt", "physical"} {
		t.Run(failure, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			plan := correctionSessionPlan(t, session)
			commands, packets, innerFinal := 0, 0, 0
			session.read = func(_ context.Context, _, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
				commands++
				if command == hbnReadBGPSummary {
					asn := uint32(65101)
					if pod == "hbn-b" {
						asn = 65102
					}
					return runtimeBGPJSON(asn, 65000), nil
				}
				return runtimeFIBJSON(prefix.String(), "p0_if"), nil
			}
			session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
				packets++
				return runtimePassedTraffic(request), nil
			}
			session.verifyPhysical = func(context.Context) error { innerFinal++; return nil }
			inner := &runtimeCorrectionObserver{open: func(context.Context) (validation.RuntimeSession, error) { return session, nil }}
			inner.snapshot = func() validation.ResourceSnapshot {
				snapshot := validation.ResourceSnapshot{}
				for _, spec := range plan.Input().Checks {
					snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: spec.ID, State: validation.Passed})
				}
				return snapshot
			}
			physicalCalls := 0
			wrapped := &deploymentAcceptanceObserver{inner: inner, scope: session.Scope(), output: func() error { return nil },
				receipts: func(context.Context) error {
					if failure == "receipt" && physicalCalls == 2 {
						return errors.New("receipt changed at closing boundary")
					}
					return nil
				},
				physical: func(context.Context) error {
					physicalCalls++
					if failure == "physical" && physicalCalls == 2 {
						return errors.New("closing physical identity unavailable")
					}
					return nil
				},
			}
			runner, err := validation.NewRunner(plan, wrapped, validation.Options{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			result := runner.Run(ctx)
			if result.Verdict == validation.VerdictPassed || result.ExitCode() == 0 || commands != 4 || packets != 2 || physicalCalls != 2 || innerFinal != 0 {
				t.Fatalf("closing %s: verdict=%s commands=%d packets=%d physical=%d innerFinal=%d", failure, result.Verdict, commands, packets, physicalCalls, innerFinal)
			}
			for _, row := range result.Checks {
				if row.CohortBound && (row.Outcome.State == validation.Passed || row.PriorOutcome == nil || row.PriorOutcome.State != validation.Passed) {
					t.Errorf("closing fence retained accepted evidence: %+v", row)
				}
			}
			if err := wrapped.fence(ctx, false); err == nil {
				t.Fatal("failed receipt session re-anchored")
			}
		})
	}
}

func TestRunAcceptanceSingleHostExceptionIsExact(t *testing.T) {
	base := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictIncomplete, Scope: validation.RuntimeScope{Hosts: []validation.HostID{"selected"}}, Report: validation.ReportDisposition{Status: validation.ReportPublished}, Checks: []validation.CheckResult{
		{CheckSpec: validation.CheckSpec{ID: "runtime/bgp/selected", Required: true}, Outcome: validation.Outcome{State: validation.Passed}},
		{CheckSpec: validation.CheckSpec{ID: "runtime/cross-host-coverage", Required: true}, Outcome: validation.Outcome{State: validation.Unverified, Reason: validation.ReasonIncomplete}},
	}}
	if !onlySingleHostCoverageIncomplete(base) {
		t.Fatal("exact one-host gap was not provisioned")
	}
	for _, state := range []validation.State{validation.Failed, validation.Unverified, validation.Pending, validation.Skipped} {
		result := acceptancePublicationSnapshot(base)
		result.Checks = append(result.Checks, validation.CheckResult{CheckSpec: validation.CheckSpec{ID: "tenant/kube-proxy", Required: true}, Outcome: validation.Outcome{State: state}})
		if onlySingleHostCoverageIncomplete(result) {
			t.Fatalf("one-host gap hid %s resource", state)
		}
	}
	for _, status := range []validation.ReportStatus{validation.ReportFailed, validation.ReportUnavailable, validation.ReportDisabled, validation.ReportPending} {
		result := base
		result.Report.Status = status
		if onlySingleHostCoverageIncomplete(result) {
			t.Fatalf("one-host gap hid %s output", status)
		}
	}
}

func TestRunAcceptancePublisherCannotMutateCanonicalEvidence(t *testing.T) {
	row := validation.Outcome{ID: "tenant/kube-proxy", State: validation.Failed, Traffic: &validation.TrafficFacts{Transmitted: 3, Received: 2}}
	result := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictFailed, Scope: validation.RuntimeScope{Hosts: []validation.HostID{"selected"}}, Checks: []validation.CheckResult{{CheckSpec: validation.CheckSpec{ID: row.ID, Required: true}, Outcome: row, PriorOutcome: &row}}, Diagnostic: &validation.Diagnostic{Summary: "canonical"}, Cause: errors.New("private-cause")}
	memory := &managementMemoryFile{}
	journal := &executionJournal{file: memory, digest: "selected-bundle"}
	err := publishDeploymentAcceptance(&result, journal, func(snapshot validation.Result) (validation.ReportDisposition, error) {
		snapshot.Checks[0].Outcome.State = validation.Passed
		snapshot.Checks[0].Outcome.Traffic.Received = 3
		snapshot.Checks[0].PriorOutcome.State = validation.Passed
		snapshot.Diagnostic.Summary = "changed"
		snapshot.Scope.Hosts[0] = "changed"
		if snapshot.Cause != nil {
			t.Error("private cause supplied to publisher")
		}
		return validation.ReportDisposition{Status: validation.ReportPublished, Path: "/safe/report.html"}, nil
	})
	if err != nil || result.Verdict != validation.VerdictFailed || result.Checks[0].Outcome.State != validation.Failed || result.Checks[0].Outcome.Traffic.Received != 2 || result.Checks[0].PriorOutcome.State != validation.Failed || result.Diagnostic.Summary != "canonical" || result.Scope.Hosts[0] != "selected" {
		t.Fatalf("publisher changed evidence: %+v %v", result, err)
	}
	data, err := json.Marshal(result)
	if err != nil || strings.Contains(string(data), "private-cause") {
		t.Fatal("private cause reached JSON")
	}
}

func TestRunAcceptancePublicationAndJournalFailuresCannotPass(t *testing.T) {
	for _, scenario := range []string{"publication", "disabled", "pending", "journal"} {
		t.Run(scenario, func(t *testing.T) {
			result := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictPassed}
			journal := &executionJournal{file: &managementMemoryFile{}, digest: "selected-bundle"}
			if scenario == "journal" {
				journal.file = &hostNetworkStageFailingJournalFile{managementMemoryFile: &managementMemoryFile{}, failSyncAt: 1}
			}
			err := publishDeploymentAcceptance(&result, journal, func(validation.Result) (validation.ReportDisposition, error) {
				switch scenario {
				case "publication":
					return validation.ReportDisposition{Status: validation.ReportFailed}, errors.New("private-publication-cause")
				case "disabled":
					return validation.ReportDisposition{Status: validation.ReportDisabled}, nil
				case "pending":
					return validation.ReportDisposition{Status: validation.ReportPending}, nil
				default:
					return validation.ReportDisposition{Status: validation.ReportPublished, Path: "/safe/report.html"}, nil
				}
			})
			if err == nil || result.ExitCode() == 0 || result.Verdict != validation.VerdictPassed {
				t.Fatalf("output failure passed or rewrote measured verdict: %+v %v", result, err)
			}
			if scenario == "journal" && !result.OutputFailed {
				t.Fatal("missing canonical durability failure")
			}
		})
	}
}

func TestRunAcceptanceCanceledLifecyclePublishesPartialHTML(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan := requireRunPlan(t, retained)
	request := imagePrerequisiteRunRequest(t, plan, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
		t.Fatal("canceled invocation reached prerequisites")
		return PrerequisiteVerification{}, nil
	})
	destination, err := validation.PrepareReportDestination(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.PublishAcceptance = func(result validation.Result) (validation.ReportDisposition, error) {
		return validation.PublishReport(destination, result, validation.ReportMetadata{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, runErr := runWithTransports(ctx, request, runTransports{resolveImage: func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
		cancel()
		return hbnPodImageIdentity{}, context.Canceled
	}})
	if runErr == nil || result.Validation == nil || result.Validation.Report.Status != validation.ReportPublished || result.TrafficValidated || result.State != RunStateFailed {
		t.Fatalf("canceled result=%+v err=%v", result, runErr)
	}
	html, err := os.ReadFile(result.Validation.Report.Path)
	if err != nil || !strings.Contains(string(html), "Full acceptance was not started") {
		t.Fatalf("partial report unavailable: %v", err)
	}
	report, err := os.ReadFile(request.ReportPath)
	if err != nil || !strings.Contains(string(report), "DEPLOYMENT_ACCEPTANCE_RESULT") {
		t.Fatal("partial canonical result was not journaled")
	}
}

func testAcceptancePublisher(t *testing.T) func(validation.Result) (validation.ReportDisposition, error) {
	t.Helper()
	destination, err := validation.PrepareReportDestination(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	published := false
	t.Cleanup(func() {
		if !published {
			_ = destination.Close()
		}
	})
	return func(result validation.Result) (validation.ReportDisposition, error) {
		published = true
		return validation.PublishReport(destination, result, validation.ReportMetadata{})
	}
}

func TestRunAcceptanceRequiresPublisherBeforeTransport(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	request := imagePrerequisiteRunRequest(t, execution.plan, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
		t.Fatal("prerequisites before reserved output")
		return PrerequisiteVerification{}, nil
	})
	request.PublishAcceptance = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := runWithTransports(ctx, request, runTransports{resolveImage: func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
		t.Fatal("image transport before reserved output")
		return hbnPodImageIdentity{}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "reserved acceptance report") {
		t.Fatalf("missing publisher accepted: %v", err)
	}
	if _, err := os.Stat(request.ReportPath); !os.IsNotExist(err) {
		t.Fatal("missing publisher created execution effects")
	}
}

func TestRunAcceptanceRemainingBudgetPrecedesRuntimeTransports(t *testing.T) {
	session, _ := standaloneValidationSession()
	observer := &deploymentAcceptanceObserver{scope: session.Scope(), inner: &runtimeCorrectionObserver{open: func(context.Context) (validation.RuntimeSession, error) {
		t.Fatal("runtime opened after insufficient remaining budget")
		return nil, nil
	}},
		receipts: func(context.Context) error {
			t.Fatal("receipt transport after insufficient runtime budget")
			return nil
		}, physical: func(context.Context) error {
			t.Fatal("physical transport after insufficient runtime budget")
			return nil
		}, output: func() error { return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := observer.OpenRuntime(ctx, nil); err == nil {
		t.Fatal("impossible final runtime budget accepted")
	}
}

func TestRunAcceptanceLegacyProjectionDoesNotInventRouteVerification(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	pair := validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"}
	result := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictFailed, Scope: validation.RuntimeScope{Traffic: []validation.DirectedHostPair{pair}}, Checks: []validation.CheckResult{
		{CheckSpec: validation.CheckSpec{ID: validation.TrafficCheckID(pair, false)}, Outcome: validation.Outcome{State: validation.Unverified, Reason: validation.ReasonIncomplete, Traffic: &validation.TrafficFacts{Transmitted: 3, Received: 3, PayloadBytes: 56}}},
		{CheckSpec: validation.CheckSpec{ID: validation.TrafficCheckID(pair, true)}, Outcome: validation.Outcome{State: validation.Unverified, Reason: validation.ReasonIncomplete, Traffic: &validation.TrafficFacts{Transmitted: 3, Received: 3, PayloadBytes: 8972}}},
	}}
	projected := projectHBNRuntime(result, f.prepared.execution.plan, f.hosts)
	if len(projected.Traffic) != 1 || projected.Traffic[0].Measurement.RouteVerified || projected.Traffic[0].Fenced {
		t.Fatalf("packet counters invented intended route proof: %+v", projected)
	}
	result.Checks[1].Outcome.State = validation.Passed
	projected = projectHBNRuntime(result, f.prepared.execution.plan, f.hosts)
	if !projected.Traffic[0].Measurement.RouteVerified {
		t.Fatal("canonical accepted traffic lost route evidence")
	}
}
