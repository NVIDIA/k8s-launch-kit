// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestCollectHBNUsesPermissionObserverWithCurrentInventory(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	previous := authorizationTestInventory("https://old.example.invalid", "old-uid")
	request := config.Request{DPFNamespace: "selected-dpf", HBN: &profile}
	request.Input.Site.Inventory = previous
	current := authorizationTestInventory("https://current.example.invalid", "current-uid")
	currentManagementBefore := *current.Management
	previousBefore := *previous
	report := &inventory.Report{
		Inventory: current,
		Discovery: inventory.Discovery{Checks: []inventory.Check{}},
	}
	calls := 0
	collectHBN(context.Background(), request, report, nil, nil, func(_ context.Context, gotRequest config.Request, gotCurrent *inventory.Inventory) []inventory.Check {
		calls++
		if gotCurrent != current {
			t.Fatal("permission observer did not receive the current inventory")
		}
		if gotRequest.Input.Site.Inventory != previous {
			t.Fatal("permission observer request lost the source inventory")
		}
		if gotCurrent.Management.APIEndpoint != "https://current.example.invalid" || gotCurrent.Management.SystemNamespaceUID != "current-uid" {
			t.Fatalf("observer received stale management identity: %#v", gotCurrent.Management)
		}
		if gotCurrent.HBN == nil || gotCurrent.HBN.Management == nil {
			t.Fatal("permission observer ran before the HBN management projection was available")
		}
		return []inventory.Check{callerPermissionCheck("dpf.hbn.management.caller-rbac.role.create", "passed")}
	})
	if calls != 1 {
		t.Fatalf("permission observer calls=%d, want 1", calls)
	}
	check := readinessCheck(t, report.Readiness.Checks, "dpf.hbn.management.caller-rbac.role.create")
	if check.State != "passed" || check.RequiredPhase != "deploy" || check.Vantage != "executor" {
		t.Fatalf("permission result was not retained in readiness: %#v", check)
	}
	if !reflect.DeepEqual(*current.Management, currentManagementBefore) || !reflect.DeepEqual(*previous, previousBefore) {
		t.Fatal("permission observation mutated current or source inventory")
	}
}

func TestCollectHBNDoesNotCallPermissionObserverForNonHBN(t *testing.T) {
	calls := 0
	report := &inventory.Report{Inventory: &inventory.Inventory{}}
	collectHBN(context.Background(), config.Request{}, report, nil, nil, func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
		calls++
		return nil
	})
	if calls != 0 || report.Inventory.HBN != nil || report.Readiness != nil {
		t.Fatalf("non-HBN collection invoked observer or added HBN output: calls=%d report=%#v", calls, report)
	}
}

func TestCollectHBNRequiresPermissionObservation(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	request := config.Request{DPFNamespace: "selected-dpf", HBN: &profile}
	report := &inventory.Report{Inventory: authorizationTestInventory("https://current.example.invalid", "current-uid")}
	collectHBN(context.Background(), request, report, nil, nil, nil)
	check := readinessCheck(t, report.Readiness.Checks, hbnCallerRBACPrefix+"observation")
	if check.State != "unknown" || check.ReasonCode != "CALLER_RBAC_OBSERVER_UNAVAILABLE" || report.Readiness.State != "blocked" {
		t.Fatalf("missing permission observer did not block HBN readiness: check=%#v readiness=%#v", check, report.Readiness)
	}
}

func TestCollectHBNConsoleSummarizesPassedCallerChecksAndKeepsFailuresDetailed(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	request := config.Request{DPFNamespace: "selected-dpf", HBN: &profile}
	current := authorizationTestInventory("https://current.example.invalid", "current-uid")
	var output bytes.Buffer
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&output))
	report := &inventory.Report{Inventory: current}
	passed := make([]inventory.Check, 100)
	for i := range passed {
		passed[i] = callerPermissionCheck(hbnCallerRBACPrefix+"role.read."+strconv.Itoa(i), "passed")
	}
	failed := callerPermissionCheck(hbnCallerRBACPrefix+"role.create.device", "failed")
	failed.Subject = "selected-dpf/dpudevices"
	failed.ReasonCode = "CALLER_PERMISSION_DENIED"
	failed.Message = "Caller lacks create permission for dpudevices.provisioning.dpu.nvidia.com in namespace selected-dpf."
	unknown := callerPermissionCheck(hbnCallerRBACPrefix+"role.list.deployment", "unknown")
	unknown.Subject = "selected-dpf/dpudeployments"
	unknown.ReasonCode = "CALLER_PERMISSION_UNVERIFIABLE"
	unknown.Message = "Could not verify list permission for dpudeployments.svc.dpu.nvidia.com in namespace selected-dpf."
	permissionChecks := append(passed, failed, unknown)
	collectHBN(ctx, request, report, nil, nil, func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
		return permissionChecks
	})
	text := output.String()
	if got := strings.Count(text, "caller authorization:"); got != 1 {
		t.Fatalf("caller permission summary appeared %d times in output:\n%s", got, text)
	}
	if !strings.Contains(text, "100 scoped permission checks passed") {
		t.Fatalf("summary does not report all passed checks:\n%s", text)
	}
	if !strings.Contains(text, "[BLOCKED] HBN-BGP \"hosts and DPUs\"") {
		t.Fatalf("existing non-RBAC readiness checks disappeared from console output:\n%s", text)
	}
	if strings.Contains(text, "role.read.") {
		t.Fatalf("individual passed caller checks were not summarized:\n%s", text)
	}
	for _, detail := range []string{
		"selected-dpf/dpudevices",
		"Caller lacks create permission for dpudevices.provisioning.dpu.nvidia.com in namespace selected-dpf.",
		"selected-dpf/dpudeployments",
		"Could not verify list permission for dpudeployments.svc.dpu.nvidia.com in namespace selected-dpf.",
	} {
		if !strings.Contains(text, detail) {
			t.Errorf("failed/unknown check detail %q missing from output:\n%s", detail, text)
		}
	}
	for _, check := range permissionChecks {
		got := readinessCheck(t, report.Readiness.Checks, check.ID)
		if got.State != check.State || got.Message != check.Message || got.ReasonCode != check.ReasonCode {
			t.Errorf("console formatting changed typed report check %q: got %#v want %#v", check.ID, got, check)
		}
	}
}

func TestHBNPermissionChecksFailClosedForUnavailableObservation(t *testing.T) {
	valid := authorizationTestInventory("https://current.example.invalid", "current-uid")
	tests := []struct {
		name     string
		ctx      context.Context
		current  *inventory.Inventory
		observer HBNPermissionObserver
		reason   string
		called   bool
	}{
		{name: "missing observer", ctx: context.Background(), current: valid, reason: "CALLER_RBAC_OBSERVER_UNAVAILABLE"},
		{name: "nil management inventory", ctx: context.Background(), observer: passingPermissionObserver(), reason: "MANAGEMENT_IDENTITY_UNAVAILABLE"},
		{name: "missing management identity", ctx: context.Background(), current: authorizationTestInventory("", "current-uid"), observer: passingPermissionObserver(), reason: "MANAGEMENT_IDENTITY_UNAVAILABLE"},
		{name: "missing namespace", ctx: context.Background(), current: valid, observer: passingPermissionObserver(), reason: "CALLER_RBAC_SCOPE_UNAVAILABLE"},
		{name: "cancelled before observation", ctx: cancelledContext(t), current: valid, observer: func(context.Context, config.Request, *inventory.Inventory) []inventory.Check { return nil }, reason: "CALLER_RBAC_INSPECTION_CANCELLED"},
		{name: "empty result", ctx: context.Background(), current: valid, observer: func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
			return []inventory.Check{}
		}, reason: "CALLER_RBAC_RESULT_EMPTY", called: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := test.ctx
			called := false
			observer := test.observer
			if observer != nil {
				original := observer
				observer = func(ctx context.Context, request config.Request, current *inventory.Inventory) []inventory.Check {
					called = true
					return original(ctx, request, current)
				}
			}
			dpfNamespace := "selected-dpf"
			if test.name == "missing namespace" {
				dpfNamespace = ""
			}
			checks := hbnPermissionChecks(ctx, config.Request{DPFNamespace: dpfNamespace}, test.current, observer)
			if called != test.called {
				t.Fatalf("observer called=%v, want %v", called, test.called)
			}
			if len(checks) != 1 || checks[0].State != "unknown" || checks[0].ReasonCode != test.reason {
				t.Fatalf("unavailable result=%#v, want one unknown %s check", checks, test.reason)
			}
		})
	}
}

func TestHBNPermissionChecksDoNotKeepAResultReturnedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	checks := hbnPermissionChecks(ctx, config.Request{DPFNamespace: "selected-dpf"}, authorizationTestInventory("https://current.example.invalid", "current-uid"), func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
		cancel()
		return []inventory.Check{callerPermissionCheck(hbnCallerRBACPrefix+"role.create", "passed")}
	})
	if len(checks) != 1 || checks[0].State != "unknown" || checks[0].ReasonCode != "CALLER_RBAC_INSPECTION_CANCELLED" {
		t.Fatalf("post-cancellation authorization result=%#v, want required unknown", checks)
	}
}

func TestHBNPermissionChecksRejectMalformedAndDuplicateResults(t *testing.T) {
	valid := callerPermissionCheck(hbnCallerRBACPrefix+"role.create", "passed")
	tests := []struct {
		name   string
		checks []inventory.Check
	}{
		{name: "wrong prefix", checks: []inventory.Check{{ID: "dpf.hbn.management.operator", Subject: "selected-dpf", Vantage: "executor", RequiredPhase: "deploy", State: "passed", ReasonCode: "OK", Message: "permission verified"}}},
		{name: "wrong phase", checks: []inventory.Check{{ID: valid.ID, Subject: valid.Subject, Vantage: valid.Vantage, RequiredPhase: "generate", State: "passed", ReasonCode: valid.ReasonCode, Message: valid.Message}}},
		{name: "wrong vantage", checks: []inventory.Check{{ID: valid.ID, Subject: valid.Subject, Vantage: "controller", RequiredPhase: "deploy", State: "passed", ReasonCode: valid.ReasonCode, Message: valid.Message}}},
		{name: "unsupported state", checks: []inventory.Check{{ID: valid.ID, Subject: valid.Subject, Vantage: valid.Vantage, RequiredPhase: valid.RequiredPhase, State: "not-evaluated", ReasonCode: valid.ReasonCode, Message: valid.Message}}},
		{name: "blank reason", checks: []inventory.Check{{ID: valid.ID, Subject: valid.Subject, Vantage: valid.Vantage, RequiredPhase: valid.RequiredPhase, State: "passed", Message: valid.Message}}},
		{name: "blank message", checks: []inventory.Check{{ID: valid.ID, Subject: valid.Subject, Vantage: valid.Vantage, RequiredPhase: valid.RequiredPhase, State: "passed", ReasonCode: valid.ReasonCode}}},
		{name: "duplicate ID", checks: []inventory.Check{valid, valid}},
		{name: "empty suffix", checks: []inventory.Check{{ID: hbnCallerRBACPrefix, Subject: valid.Subject, Vantage: valid.Vantage, RequiredPhase: valid.RequiredPhase, State: "passed", ReasonCode: valid.ReasonCode, Message: valid.Message}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := hbnPermissionChecks(context.Background(), config.Request{DPFNamespace: "selected-dpf"}, authorizationTestInventory("https://current.example.invalid", "current-uid"), func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
				return test.checks
			})
			if len(checks) != 1 || checks[0].State != "unknown" || checks[0].ReasonCode != "CALLER_RBAC_RESULT_INVALID" {
				t.Fatalf("malformed callback result=%#v", checks)
			}
		})
	}
}

func TestHBNPermissionChecksPreserveDeniedAndUnknownReasons(t *testing.T) {
	for _, state := range []string{"failed", "unknown"} {
		want := callerPermissionCheck(hbnCallerRBACPrefix+"role.create", state)
		got := hbnPermissionChecks(context.Background(), config.Request{DPFNamespace: "selected-dpf"}, authorizationTestInventory("https://current.example.invalid", "current-uid"), func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
			return []inventory.Check{want}
		})
		if !reflect.DeepEqual(got, []inventory.Check{want}) {
			t.Fatalf("%s authorization result changed: got %#v want %#v", state, got, want)
		}
	}
}

func TestManagementHBNChecksDoNotClaimCallerRBACWasObserved(t *testing.T) {
	checks, _ := managementHBNChecks(nil, hbnManagementTestInventoryPointer(), "dpf-operator-system")
	for _, check := range checks {
		if strings.Contains(check.ID, "rbac") {
			t.Fatalf("pure management facts still emit a caller RBAC check: %#v", check)
		}
	}
}

func authorizationTestInventory(endpoint, uid string) *inventory.Inventory {
	return &inventory.Inventory{Management: &inventory.Management{
		APIEndpoint: endpoint, SystemNamespaceUID: uid, TrustDigest: "sha256:current-trust",
	}}
}

func callerPermissionCheck(id, state string) inventory.Check {
	return inventory.Check{
		ID: id, Subject: "selected-dpf", Vantage: "executor", RequiredPhase: "deploy", State: state,
		ReasonCode: "CALLER_PERMISSION_VERIFIED", Message: "The configured identity has the required scoped permission.",
	}
}

func passingPermissionObserver() HBNPermissionObserver {
	return func(context.Context, config.Request, *inventory.Inventory) []inventory.Check {
		return []inventory.Check{callerPermissionCheck(hbnCallerRBACPrefix+"role.create", "passed")}
	}
}

func readinessCheck(t *testing.T, checks []inventory.Check, id string) inventory.Check {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing readiness check %q", id)
	return inventory.Check{}
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
