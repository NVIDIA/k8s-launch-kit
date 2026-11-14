// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const stagedOSBytes = "selected operating system image"
const stagedPLDMBytes = "selected firmware bundle"

type softwareStageFake struct {
	*enrollmentStageFake
	server          *httptest.Server
	managerImage    string
	permissionState string
	createSoftware  int
	registryReads   int
	streamRequests  atomic.Int32
	tunnelCloses    int
	streamMismatch  atomic.Bool
	cleanupError    error
}

func (f *softwareStageFake) Create(ctx context.Context, desired *unstructured.Unstructured, dry bool) (*unstructured.Unstructured, error) {
	created, err := f.enrollmentStageFake.Create(ctx, desired, dry)
	if err != nil || created.GetKind() != "BlueFieldSoftware" {
		return created, err
	}
	f.createSoftware++
	created.SetUID(types.UID("software-uid"))
	created.SetResourceVersion("9")
	created.SetGeneration(1)
	osBase, err := dpfSoftwareFilename(f.base.execution.plan.Intent().Software.OSISO.URL)
	if err != nil {
		return nil, err
	}
	pldm := f.base.execution.plan.Intent().Software.PLDM["MT_0000001774"]
	pldmBase, err := dpfSoftwareFilename(pldm.URL)
	if err != nil {
		return nil, err
	}
	_ = unstructured.SetNestedField(created.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedField(created.Object, int64(1), "status", "observedGeneration")
	_ = unstructured.SetNestedField(created.Object, map[string]any{
		"osIso":        "/bfb/" + created.GetNamespace() + "-" + created.GetName() + "-" + osBase,
		"pldmFwBundle": map[string]any{"MT_0000001774": "/bfb/components/" + created.GetNamespace() + "-" + created.GetName() + "-MT_0000001774-" + pldmBase},
	}, "status", "downloadedComponents")
	_ = unstructured.SetNestedSlice(created.Object, []any{
		conditionMap("Downloaded", "True", "Success", 1),
		conditionMap("Ready", "True", "Success", 1),
	}, "status", "conditions")
	f.base.resources[tenantStageKey(created)] = created.DeepCopy()
	return created, nil
}

func (f *softwareStageFake) checkSoftwarePermissions(context.Context, *unstructured.Unstructured, string) []PreflightCheck {
	state := f.permissionState
	if state == "" {
		state = "passed"
	}
	return []PreflightCheck{{ID: "software.create", State: state, ReasonCode: "SOFTWARE_PERMISSION_CHECKED", Message: "permission result"}}
}

func (f *softwareStageFake) observeRegistrySnapshot(_ context.Context, namespace string) (registrySnapshot, error) {
	f.registryReads++
	return registrySnapshot{namespace: namespace, manager: registryManagerBinding{container: registryContainerBinding{image: f.managerImage}}}, nil
}

func (f *softwareStageFake) openSoftwareRegistry(_ context.Context, _ string, _ []byte) (*registryTunnel, error) {
	parsed, err := url.Parse(f.server.URL)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return nil, err
	}
	return &registryTunnel{port: port, client: f.server.Client(), closeFn: func() error { f.tunnelCloses++; return f.cleanupError }}, nil
}

func softwareStageFixture(t *testing.T) (*managementExecution, controlPlaneReceipts, []enrollmentBMC, string, *softwareStageFake, *executionJournal) {
	t.Helper()
	expected, sources := tenantSourceFixture(t)
	input := preflightGenerationInputs(t)
	input.Operator.Bootstrap.Tenant = bootstrap.Tenant{Name: expected.Name, Namespace: expected.Namespace, Labels: expected.Labels, MaxNodes: int(expected.MaxNodes), VIP: expected.VIP, Interface: expected.Interface, RouterID: int(expected.VirtualRouterID), NodeSelector: expected.NodeSelector, ServiceCIDR: expected.ServiceCIDR}
	input.Operator.Bootstrap.PodCIDR = expected.PodCIDR
	input.Operator.Bootstrap.Devices[0].BMCIP = "192.0.2.30"
	input.Workload.DPUClusterSelector = expected.Labels
	setSiteAllocation(&input.Operator.Bootstrap.Tenant)
	input.Workload.Software.OSISO.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(stagedOSBytes)))
	pldm := input.Workload.Software.PLDM["MT_0000001774"]
	pldm.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(stagedPLDMBytes)))
	input.Workload.Software.PLDM["MT_0000001774"] = pldm
	generated, err := generation.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := controlPlaneTestRequest(t)
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://192.0.2.30"
		}
	}
	selected, password, err := bindEnrollmentBMCs(execution.plan.Intent().Devices, request.Targets)
	if err != nil {
		t.Fatal(err)
	}
	source := &tenantSourceStub{sources: sources}
	tenantClientValue, _ := tenantIdentityClientFixture(t, expected, source)
	base := newTenantStageFake(t, execution)
	fake := &enrollmentStageFake{base: base, operator: newManagementFake(t, generated), sources: source, selected: selected}
	tenantClientValue.source = fake
	management := &softwareStageFake{enrollmentStageFake: fake, managerImage: "nvcr.io/nvidia/doca/dpf-system:v26.8.0"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		management.streamRequests.Add(1)
		data := stagedOSBytes
		if strings.HasPrefix(r.URL.Path, "/bfb/components/") {
			data = stagedPLDMBytes
		}
		if management.streamMismatch.Load() {
			data += " changed"
		}
		_, _ = io.WriteString(w, data)
	}))
	management.server = server
	t.Cleanup(server.Close)
	tenantClientValue.source = management
	managementReceipt := tenantStageBootstrapReceipt(execution, base)
	tenant := tenantStageReceipt{ClusterUID: "dc-uid", Client: tenantClientValue}
	enrollment, err := runEnrollmentStage(tenantStageContext(t), management, execution, managementReceipt, tenant, selected, password, testJournal(execution), time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("prepare actual enrolled fixture: %v", err)
	}
	management.bmcCalls = 0
	management.base.events = nil
	management.base.createCount = 0
	receipts := controlPlaneReceipts{management: managementReceipt, tenant: tenant, enrollment: enrollment}
	return execution, receipts, selected, password, management, testJournal(execution)
}

func TestRunSoftwareStageCreatesWaitsAndVerifiesRetainedCache(t *testing.T) {
	execution, receipts, selected, password, fake, journal := softwareStageFixture(t)
	got, err := runSoftwareStage(tenantStageContext(t), fake, execution, receipts, selected, password, journal, time.Millisecond, time.Second, 1<<20)
	if err != nil {
		t.Fatalf("runSoftwareStage() error = %v", err)
	}
	if got.SoftwareUID != "software-uid" || len(got.Components) != 2 || fake.createSoftware != 1 || fake.streamRequests.Load() != 2 || fake.tunnelCloses != 1 || fake.registryReads != 3 || fake.bmcCalls != 2 {
		t.Fatalf("receipt=%#v creates=%d streams=%d closes=%d registryReads=%d", got, fake.createSoftware, fake.streamRequests.Load(), fake.tunnelCloses, fake.registryReads)
	}
}

func TestRunSoftwareStageStopsBeforeCreateOnReceiptPermissionOrManagerMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*controlPlaneReceipts, *softwareStageFake)
	}{
		{name: "receipt digest", mutate: func(r *controlPlaneReceipts, _ *softwareStageFake) { r.enrollment.BundleDigest = "different" }},
		{name: "tenant source", mutate: func(r *controlPlaneReceipts, _ *softwareStageFake) { r.tenant.Client.source = &tenantSourceStub{} }},
		{name: "device replacement", mutate: func(_ *controlPlaneReceipts, f *softwareStageFake) {
			for _, obj := range f.base.resources {
				if obj.GetKind() == "DPUDevice" {
					obj.SetUID("replacement")
				}
			}
		}},
		{name: "permission", mutate: func(_ *controlPlaneReceipts, f *softwareStageFake) { f.permissionState = "failed" }},
		{name: "manager release", mutate: func(_ *controlPlaneReceipts, f *softwareStageFake) { f.managerImage = "example.test/other:tag" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution, receipts, selected, password, fake, journal := softwareStageFixture(t)
			tc.mutate(&receipts, fake)
			_, err := runSoftwareStage(tenantStageContext(t), fake, execution, receipts, selected, password, journal, time.Millisecond, time.Second, 1<<20)
			if err == nil || fake.createSoftware != 0 {
				t.Fatalf("stage error=%v software creates=%d", err, fake.createSoftware)
			}
		})
	}
}

func TestRunSoftwarePreparationRejectsInvalidByteLimitsBeforeManagement(t *testing.T) {
	for _, limit := range []int64{0, 1<<63 - 1} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		journal := &executionJournal{file: &managementMemoryFile{}, digest: strings.Repeat("a", 64)}
		_, err := runSoftwarePreparation(ctx, &softwareStageFake{}, nil, journal, probe.Runner{}, config.Request{}, time.Second, time.Second, limit)
		cancel()
		if err == nil {
			t.Fatalf("byte limit %d accepted", limit)
		}
	}
}

func TestSoftwareStageFailuresAfterCreateCannotIssueReceipt(t *testing.T) {
	for _, scenario := range []string{"digest", "cleanup", "final-device-replacement"} {
		t.Run(scenario, func(t *testing.T) {
			execution, receipts, selected, password, fake, journal := softwareStageFixture(t)
			switch scenario {
			case "digest":
				fake.streamMismatch.Store(true)
			case "cleanup":
				fake.cleanupError = errors.New("tunnel cleanup failed")
			case "final-device-replacement":
				fake.getHook = func(obj *unstructured.Unstructured) {
					if obj.GetKind() == "DPUDevice" && fake.registryReads >= 3 {
						fake.base.resources[tenantStageKey(obj)].SetUID("replacement")
					}
				}
			}
			got, err := runSoftwareStage(tenantStageContext(t), fake, execution, receipts, selected, password, journal, time.Millisecond, time.Second, 1<<20)
			if err == nil || got.SoftwareUID != "" || len(got.Components) != 0 || fake.createSoftware != 1 || fake.tunnelCloses != 1 {
				t.Fatalf("receipt=%#v creates=%d close=%d err=%v", got, fake.createSoftware, fake.tunnelCloses, err)
			}
			report := journal.file.(*managementMemoryFile).String()
			if strings.Contains(report, "SOFTWARE_BYTES_VERIFIED") || strings.Contains(report, password) {
				t.Fatal("failed run reported completed verification or leaked credentials")
			}
			for _, event := range fake.base.events {
				if strings.HasPrefix(event, "create:") && event != "create:BlueFieldSoftware" {
					t.Fatalf("unexpected dependent effect: %s", event)
				}
			}
		})
	}
}

func TestSoftwareStageCancellationDuringFinalReceiptCannotReturnSuccess(t *testing.T) {
	execution, receipts, selected, password, fake, journal := softwareStageFixture(t)
	ctx, cancel := context.WithCancel(tenantStageContext(t))
	defer cancel()
	report := &cancelPendingEnrollmentFile{cancel: cancel, reason: "SOFTWARE_BYTES_VERIFIED"}
	journal.file = report
	got, err := runSoftwareStage(ctx, fake, execution, receipts, selected, password, journal, time.Millisecond, time.Second, 1<<20)
	if !errors.Is(err, context.Canceled) || got.SoftwareUID != "" || len(got.Components) != 0 || fake.tunnelCloses != 1 {
		t.Fatalf("receipt=%#v closes=%d err=%v", got, fake.tunnelCloses, err)
	}
	if !strings.Contains(report.String(), "SOFTWARE_VERIFICATION_CANCELED") {
		t.Fatal("cancellation was not persisted")
	}
}
