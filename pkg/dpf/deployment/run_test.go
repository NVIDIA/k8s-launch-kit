// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestRunRejectsIncompleteBoundaryAndUnboundedControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request RunRequest
	}{
		{name: "missing bundle"},
		{name: "missing callbacks", request: RunRequest{ReportPath: "report", ProbeJournalPath: "probe"}},
		{name: "invalid control", request: RunRequest{PollInterval: 2 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRunRequest(tc.request); err == nil {
				t.Fatal("incomplete or unbounded run request was accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, RunRequest{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("run without bounded deadline = %v", err)
	}
}

func TestRunDefaultSoftwareLimitAdmitsLargeReleaseWhilePreservingExplicitBound(t *testing.T) {
	// Release media can exceed 4 GiB. Advertise that size without transferring
	// it: the accepted header must reach stream verification, which still fails
	// on this deliberately empty body. A small explicit limit must stop earlier.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt((4<<30)+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, test := range []struct {
		name string
		max  int64
		want string
	}{
		{name: "default admits release media", max: 0, want: "stream ended before verification completed"},
		{name: "explicit smaller cap remains authoritative", max: 1 << 30, want: "exceeds the verification limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, maximum := normalizeRunControls(RunRequest{MaxSoftwareBytes: test.max})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := verifyRegistryStream(ctx, server.Client(), server.URL+"/release.iso", strings.Repeat("a", 64), maximum); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("software header/body boundary error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRunPersistsPrerequisiteFailureBeforeManagementAccess(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	reportPath := filepath.Join(root, "run.jsonl")
	probePath := filepath.Join(root, "probe.jsonl")
	marker := errors.New("observer sentinel")
	called := false
	target := config.Target{Role: "dpus", Endpoint: "https://192.0.2.10:443", InputRef: "dpu-input"}
	var request RunRequest
	request = RunRequest{
		Bundle:     generated,
		Access:     config.Request{Targets: []config.Target{target}, Kubeconfig: filepath.Join(root, "missing-kubeconfig")},
		ReportPath: reportPath, ProbeJournalPath: probePath,
		VerifyPrerequisites: func(ctx context.Context, observed config.Request, plan *generation.Plan) (PrerequisiteVerification, error) {
			called = true
			if ctx == nil || plan == nil || plan.Digest() != generated.Index().BundleDigest {
				t.Fatal("observer did not receive the bounded context and validated retained plan")
			}
			observed.Targets[0].Endpoint = "https://192.0.2.11:443"
			if observed.Targets[0].Endpoint == request.Access.Targets[0].Endpoint {
				t.Fatal("observer request mutation did not take effect in the isolated copy")
			}
			return PrerequisiteVerification{Checks: []inventory.Check{{
				ID: "dpf.hbn.management.caller-rbac", Subject: "dpf-operator-system", Vantage: "management-cluster",
				RequiredPhase: "deploy", State: "unknown", ReasonCode: "PERMISSION_OBSERVATION_INCOMPLETE",
				Message: "The current caller permission check was incomplete.", EvidenceRefs: []string{"internal-only"},
			}}}, marker
		},
		VerifyChartSource: func(context.Context, *generation.Plan) (PreflightCheck, error) {
			t.Fatal("chart-source callback ran before prerequisites")
			return PreflightCheck{}, nil
		},
		Rebooter: RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
			t.Fatal("reboot callback ran before prerequisites")
			return ActivationObservation{}, nil
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, runErr := Run(ctx, request)
	if runErr == nil || !strings.Contains(runErr.Error(), "prerequisite observation failed") || !called {
		t.Fatalf("Run = %#v, %v; observer called=%v", result, runErr, called)
	}
	if request.Access.Targets[0].Endpoint != target.Endpoint {
		t.Fatalf("observer mutated caller-owned request: %q", request.Access.Targets[0].Endpoint)
	}
	if result.State != "failed" || result.BundleDigest != generated.Index().BundleDigest || len(result.Checks) < 2 ||
		result.Checks[0].State != "unknown" || result.Checks[0].ReasonCode != "PERMISSION_OBSERVATION_INCOMPLETE" {
		t.Fatalf("failure result did not preserve its safe prerequisite state: %#v", result)
	}
	if len(result.Checks[0].EvidenceRefs) != 0 {
		t.Fatal("run result exposed prerequisite evidence references")
	}
	if result.TrafficValidated {
		t.Fatal("failed run reported traffic validation")
	}
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), marker.Error()) {
		t.Fatal("raw observer error was persisted in the execution report")
	}
	var events []executionEvent
	for _, line := range strings.Split(strings.TrimSpace(string(report)), "\n") {
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) < 2 || events[0].ReasonCode != "EXECUTION_STARTED" || events[len(events)-1].State != "failed" {
		t.Fatalf("execution report lacks durable start/failure records: %#v", events)
	}
	probeReport, err := os.ReadFile(probePath)
	if err != nil || !strings.Contains(string(probeReport), `"receipts":[]`) {
		t.Fatalf("probe journal was not initialized before prerequisites: %q, %v", probeReport, err)
	}
}

func TestValidatePrerequisiteVerificationRequiresCompleteDeployChecks(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	intent := plan.Intent()
	current := &inventory.Inventory{
		FormatVersion: inventory.FormatVersion,
		Management: &inventory.Management{
			APIEndpoint: intent.Management.APIEndpoint, SystemNamespaceUID: intent.Management.SystemNamespaceUID,
			TrustDigest: intent.Management.TrustDigest, KubernetesVersion: intent.KubernetesVersion,
		},
	}
	verification := PrerequisiteVerification{Inventory: current, Checks: []inventory.Check{{
		ID: "caller.permission", Subject: "dpf-operator-system", Vantage: "management-cluster", RequiredPhase: "deploy", State: "passed",
	}}}
	if err := validatePrerequisiteVerification(plan, verification); err != nil {
		t.Fatalf("valid complete prerequisite observation: %v", err)
	}
	for _, mutate := range []struct {
		name string
		fn   func(*PrerequisiteVerification)
	}{
		{name: "no deploy check", fn: func(v *PrerequisiteVerification) { v.Checks[0].RequiredPhase = "generate" }},
		{name: "unknown without reason", fn: func(v *PrerequisiteVerification) { v.Checks[0].State = "unknown" }},
		{name: "duplicate identity", fn: func(v *PrerequisiteVerification) { v.Checks = append(v.Checks, v.Checks[0]) }},
		{name: "inventory mismatch", fn: func(v *PrerequisiteVerification) { v.Inventory.Management.TrustDigest = "other" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			candidateInventory, err := cloneInventory(current)
			if err != nil {
				t.Fatal(err)
			}
			candidate := PrerequisiteVerification{Inventory: candidateInventory, Checks: append([]inventory.Check(nil), verification.Checks...)}
			mutate.fn(&candidate)
			if err := validatePrerequisiteVerification(plan, candidate); err == nil {
				t.Fatal("invalid prerequisite observation was accepted")
			}
		})
	}
}

func TestValidateSelectedInventoryBindsExactSelectedEndpoints(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	intent := plan.Intent()
	current := &inventory.Inventory{FormatVersion: inventory.FormatVersion}
	for _, device := range intent.Devices {
		current.DPUs = append(current.DPUs, inventory.Device{
			SerialNumber: device.Serial, BMCEndpoint: (&url.URL{Scheme: "https", Host: device.BMCIP + ":" + strconv.Itoa(device.BMCPort)}).String(),
			ObservationState: "observed", ID: "observed-dpu",
		})
	}
	if err := validateSelectedInventory(plan, current); err != nil {
		t.Fatalf("exact selected inventory rejected: %v", err)
	}
	current.DPUs[0].BMCEndpoint = "https://192.0.2.250:443"
	if err := validateSelectedInventory(plan, current); err == nil {
		t.Fatal("changed selected BMC endpoint was accepted")
	}
}

func TestCloneInventoryPreservesTransientNodeLabels(t *testing.T) {
	input := &inventory.Inventory{Management: &inventory.Management{Nodes: []inventory.Node{{Name: "node-a", Labels: map[string]string{"role": "control-plane"}}}}}
	cloned, err := cloneInventory(input)
	if err != nil {
		t.Fatal(err)
	}
	cloned.Management.Nodes[0].Labels["role"] = "changed"
	if input.Management.Nodes[0].Labels["role"] != "control-plane" {
		t.Fatal("clone shares transient Kubernetes selector labels with caller")
	}
}

type runtimeCloseFailFile struct{ *managementMemoryFile }

func (f *runtimeCloseFailFile) Close() error {
	return errors.New("simulated durable report close failure")
}

func TestRunRuntimeFinalDurabilityGate(t *testing.T) {
	for _, scenario := range []string{"validated", "one-host-unmeasured", "stage-failed", "result-sync-failed", "report-close-failed", "probe-close-failed"} {
		t.Run(scenario, func(t *testing.T) {
			runtime := &HBNRuntimeResult{State: "passed", TrafficValidated: true, BGP: []HBNBGPResult{{DPUSerial: "SERIAL-A", State: "passed", Fenced: true}}}
			result := RunResult{State: RunStateFailed, LastCompletedStage: hbnRuntimeStage, TrafficValidated: true, HBNRuntime: runtime,
				HostNetworking: []HostWorkloadNetworkResult{{DPUSerial: "SERIAL-A", LeaseReverified: true}}, Activation: &ActivationSummary{DPUDeploymentUID: "retained-deployment"}}
			memory := &managementMemoryFile{}
			journal := &executionJournal{file: memory, digest: "retained-bundle"}
			closeProbe := func() error { return nil }
			var stageErr error
			switch scenario {
			case "one-host-unmeasured":
				result.TrafficValidated = false
				runtime.TrafficValidated = false
				runtime.State = "unmeasured"
				result.LastCompletedStage = hostWorkloadNetworkStage
			case "stage-failed":
				stageErr = errors.New("HBN_RUNTIME_POST_FENCE_FAILED")
				result.TrafficValidated = false
				runtime.TrafficValidated = false
				runtime.State = "failed"
				result.LastCompletedStage = hostWorkloadNetworkStage
			case "result-sync-failed":
				journal.file = &hostNetworkStageFailingJournalFile{managementMemoryFile: memory, failSyncAt: 1}
			case "report-close-failed":
				journal.file = &runtimeCloseFailFile{managementMemoryFile: memory}
			case "probe-close-failed":
				closeProbe = func() error { return errors.New("probe close failure") }
			}
			err := finishRunReport(&result, stageErr, closeProbe, journal, "selected-workloads")
			switch scenario {
			case "validated":
				if err != nil || result.State != RunStateValidated || !result.TrafficValidated || !result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hbnRuntimeStage {
					t.Fatalf("validated result=%+v err=%v", result, err)
				}
			case "one-host-unmeasured":
				if err != nil || result.State != RunStateProvisioned || result.TrafficValidated || result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hostWorkloadNetworkStage {
					t.Fatalf("one host result=%+v err=%v", result, err)
				}
			default:
				if err == nil || result.State != RunStateFailed || result.TrafficValidated || result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hostWorkloadNetworkStage || result.Checks[len(result.Checks)-1].State != "failed" {
					t.Fatalf("uncertain result=%+v err=%v", result, err)
				}
			}
			if len(result.HostNetworking) != 1 || !result.HostNetworking[0].LeaseReverified || result.Activation.DPUDeploymentUID != "retained-deployment" || len(result.HBNRuntime.BGP) != 1 {
				t.Fatal("finalization lost prior partial receipts")
			}
		})
	}
}
