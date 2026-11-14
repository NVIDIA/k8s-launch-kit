// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type enrollmentStageFake struct {
	*APIClient
	base     *tenantStageFake
	operator *managementFake
	sources  *tenantSourceStub
	selected []enrollmentBMC
	bmcCalls int
	bmcHook  func(int) error
	getHook  func(*unstructured.Unstructured)
	scanHook func(*unstructured.Unstructured)
}

func (f *enrollmentStageFake) VerifyIdentity(ctx context.Context) error {
	return f.base.VerifyIdentity(ctx)
}
func (f *enrollmentStageFake) matchesBinding(id generation.ManagementIdentity, version string) bool {
	return f.base.matchesBinding(id, version)
}
func (f *enrollmentStageFake) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if f.getHook != nil {
		f.getHook(wanted)
	}
	return f.base.Get(ctx, wanted)
}
func (f *enrollmentStageFake) Create(ctx context.Context, desired *unstructured.Unstructured, dry bool) (*unstructured.Unstructured, error) {
	created, err := f.base.Create(ctx, desired, dry)
	if err != nil {
		return nil, err
	}
	created.SetUID(types.UID(created.GetKind() + "-" + created.GetName()))
	created.SetGeneration(1)
	if created.GetKind() == "DPUDiscovery" {
		created.Object["status"] = map[string]any{"observedGeneration": int64(1), "lastScanTime": "2026-11-14T00:00:00Z", "foundDPUs": int64(7)}
		for _, item := range f.selected {
			for _, obj := range readyEnrollmentObjects(item, f.base.execution.operatorNamespace) {
				if f.base.resources[tenantStageKey(obj)] == nil {
					f.base.resources[tenantStageKey(obj)] = obj
				}
			}
		}
		if f.scanHook != nil {
			f.scanHook(created)
		}
	}
	f.base.resources[tenantStageKey(created)] = created
	return created.DeepCopy(), nil
}
func (f *enrollmentStageFake) List(_ context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if wanted.GetNamespace() != "" || selector != "" {
		return nil, fmt.Errorf("scope must list all namespaces without filtering")
	}
	result := []unstructured.Unstructured{}
	for _, obj := range f.base.resources {
		if obj.GetKind() == wanted.GetKind() {
			result = append(result, *obj.DeepCopy())
		}
	}
	return result, nil
}
func (f *enrollmentStageFake) getOperatorCRD(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return f.operator.Get(ctx, &unstructured.Unstructured{Object: map[string]any{"kind": "CustomResourceDefinition", "metadata": map[string]any{"name": name}}})
}
func (f *enrollmentStageFake) discoverResourceList(ctx context.Context, gv string) (*metav1.APIResourceList, error) {
	return f.operator.discoverResourceList(ctx, gv)
}
func (f *enrollmentStageFake) observeTenantSources(ctx context.Context, e TenantReadinessExpected) (tenantSources, error) {
	return f.sources.observeTenantSources(ctx, e)
}
func (f *enrollmentStageFake) verifyEnrollmentBMCs(_ context.Context, _ []enrollmentBMC, _ time.Duration, _ *executionJournal) error {
	f.bmcCalls++
	f.base.events = append(f.base.events, "bmc")
	if f.bmcHook != nil {
		return f.bmcHook(f.bmcCalls)
	}
	return nil
}
func (f *enrollmentStageFake) executeEnrollmentSecret(ctx context.Context, c ResourceClient, ns, pw string, j *executionJournal) (enrollmentSecretReceipt, error) {
	return executeEnrollmentSecret(ctx, c, ns, pw, j)
}

func enrollmentStageFixture(t *testing.T) (*managementExecution, *enrollmentStageFake, managementBootstrapReceipt, tenantStageReceipt, string) {
	t.Helper()
	expected, sources := tenantSourceFixture(t)
	input := preflightGenerationInputs(t)
	input.Operator.Bootstrap.Tenant = bootstrap.Tenant{Name: expected.Name, Namespace: expected.Namespace, Labels: expected.Labels, MaxNodes: int(expected.MaxNodes), VIP: expected.VIP, Interface: expected.Interface, RouterID: int(expected.VirtualRouterID), NodeSelector: expected.NodeSelector, ServiceCIDR: expected.ServiceCIDR}
	input.Operator.Bootstrap.PodCIDR = expected.PodCIDR
	input.Operator.Bootstrap.Devices[0].BMCIP = "192.0.2.30"
	input.Workload.DPUClusterSelector = expected.Labels
	setSiteAllocation(&input.Operator.Bootstrap.Tenant)
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
	tenant, _ := tenantIdentityClientFixture(t, expected, source)
	base := newTenantStageFake(t, execution)
	fake := &enrollmentStageFake{base: base, operator: newManagementFake(t, generated), sources: source, selected: selected}
	tenant.source = fake
	return execution, fake, tenantStageBootstrapReceipt(execution, base), tenantStageReceipt{ClusterUID: "dc-uid", Client: tenant}, password
}

func readyEnrollmentObjects(item enrollmentBMC, namespace string) []*unstructured.Unstructured {
	name := strings.ToLower(item.device.Serial)
	device := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUDevice", "metadata": map[string]any{"name": name, "namespace": namespace, "uid": "device-" + name, "resourceVersion": "1", "generation": int64(1)}, "spec": map[string]any{"serialNumber": item.device.Serial, "bmcIp": item.device.BMCIP, "bmcPort": int64(item.device.BMCPort), "bmcFactoryResetPolicy": "Never"}, "status": map[string]any{"serialNumber": item.device.Serial, "bmcIp": item.device.BMCIP, "bmcPort": int64(item.device.BMCPort), "psid": item.device.PSID, "dpuType": "BlueField4"}}}
	conditions := []any{}
	for _, typ := range []string{"Initialized", "Discovered", "Ready", "BMCCredentialsReady", "BMCServerCertificateReady", "CATrustBundleReady", "BMCFactoryResetReady"} {
		reason := "Success"
		if typ == "BMCFactoryResetReady" {
			reason = "FactoryResetSkipped"
		}
		conditions = append(conditions, conditionMap(typ, "True", reason, 1))
	}
	_ = unstructured.SetNestedSlice(device.Object, conditions, "status", "conditions")
	node := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNode", "metadata": map[string]any{"name": "dpu-node-" + name, "namespace": namespace, "uid": "node-" + name, "resourceVersion": "1", "generation": int64(1)}, "spec": map[string]any{"dpus": []any{map[string]any{"name": name}}, "nodeRebootMethod": map[string]any{"external": map[string]any{}}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Success"}}}}}
	return []*unstructured.Unstructured{device, node}
}

func TestEnrollmentStageCreatesOnlySelectedScanAndCredential(t *testing.T) {
	execution, fake, bootstrapReceipt, tenantReceipt, password := enrollmentStageFixture(t)
	journal := testJournal(execution)
	receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution, bootstrapReceipt, tenantReceipt, fake.selected, password, journal, time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Devices) != 1 || receipt.Devices[0].DeviceUID != "device-serial-a" || receipt.Devices[0].DPUNodeUID != "node-serial-a" || !receipt.Secret.Created || fake.base.createCount != 2 {
		t.Fatalf("receipt=%+v creates=%d", receipt, fake.base.createCount)
	}
	requireOrderedTenantEvents(t, fake.base.events, "bmc", "create:Secret", "bmc", "create:DPUDiscovery", "bmc")
	if strings.Contains(journal.file.(*managementMemoryFile).String(), password) {
		t.Fatal("password leaked")
	}
}

func TestEnrollmentStageStopsAtChangedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*enrollmentStageFake, *tenantStageReceipt)
		want    string
		creates int
	}{
		{name: "wrong tenant expectation", edit: func(_ *enrollmentStageFake, r *tenantStageReceipt) { r.Client.expected.VIP = "192.0.2.99" }, want: "expected configuration", creates: 0},
		{name: "admin unavailable", edit: func(f *enrollmentStageFake, _ *tenantStageReceipt) {
			f.bmcHook = func(int) error { return fmt.Errorf("admin no longer verified") }
		}, want: "admin", creates: 0},
		{name: "secret replaced before scan", edit: func(f *enrollmentStageFake, _ *tenantStageReceipt) {
			f.bmcHook = func(n int) error {
				if n == 2 {
					f.base.resources[tenantStageKey(secretIdentity(f.base.execution.operatorNamespace))].SetUID("replacement")
				}
				return nil
			}
		}, want: "ENROLLMENT_SECRET_CHANGED", creates: 1},
		{name: "tenant replaced before receipt", edit: func(f *enrollmentStageFake, _ *tenantStageReceipt) {
			f.scanHook = func(_ *unstructured.Unstructured) { f.sources.sources.secret.UID = "replacement" }
		}, want: "TENANT_API_IDENTITY_CHANGED", creates: 2},
		{name: "secret replaced before receipt", edit: func(f *enrollmentStageFake, _ *tenantStageReceipt) {
			f.bmcHook = func(n int) error {
				if n == 3 {
					f.base.resources[tenantStageKey(secretIdentity(f.base.execution.operatorNamespace))].SetUID("replacement")
				}
				return nil
			}
		}, want: "ENROLLMENT_SECRET_CHANGED", creates: 2},
		{name: "scan specification changed", edit: func(f *enrollmentStageFake, _ *tenantStageReceipt) {
			f.getHook = func(w *unstructured.Unstructured) {
				if w.GetKind() == "DPUDiscovery" {
					if o := f.base.resources[tenantStageKey(w)]; o != nil {
						_ = unstructured.SetNestedField(o.Object, "192.0.2.99", "spec", "ipRangeSpec", "ipRange", "endIP")
					}
				}
			}
		}, want: "DPU_DISCOVERY_INTENT_CHANGED", creates: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution, fake, br, tr, password := enrollmentStageFixture(t)
			tc.edit(fake, &tr)
			receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution, br, tr, fake.selected, password, testJournal(execution), time.Millisecond, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) || fake.base.createCount != tc.creates {
				t.Fatalf("err=%v creates=%d", err, fake.base.createCount)
			}
			if tc.creates > 0 && receipt.Secret.UID == "" {
				t.Fatal("partial Secret receipt discarded")
			}
		})
	}
}

func TestEnrollmentCohortNeverCachesReadyDevice(t *testing.T) {
	for _, change := range []string{"replace", "lose-readiness", "missing-node", "status-mismatch"} {
		t.Run(change, func(t *testing.T) {
			execution, fake, _, _, _ := enrollmentStageFixture(t)
			second := fake.selected[0]
			second.device.Serial = "SERIAL-B"
			second.device.BMCIP = "192.0.2.31"
			selected := append(append([]enrollmentBMC{}, fake.selected...), second)
			for _, item := range selected {
				for _, obj := range readyEnrollmentObjects(item, execution.operatorNamespace) {
					fake.base.resources[tenantStageKey(obj)] = obj
				}
			}
			journal := testJournal(execution)
			initial, allReady, err := observeSelectedEnrollment(tenantStageContext(t), fake, execution, selected, make([]enrolledDeviceReceipt, 2), journal)
			if err != nil || !allReady {
				t.Fatalf("initial ready cohort: %v %t", err, allReady)
			}
			first := fake.base.resources["DPUDevice/"+execution.operatorNamespace+"/serial-a"]
			switch change {
			case "replace":
				first.SetUID("replacement")
			case "lose-readiness":
				_ = unstructured.SetNestedSlice(first.Object, []any{conditionMap("Ready", "False", "Initializing", 1)}, "status", "conditions")
			case "missing-node":
				delete(fake.base.resources, "DPUNode/"+execution.operatorNamespace+"/dpu-node-serial-a")
			case "status-mismatch":
				_ = unstructured.SetNestedField(first.Object, "SERIAL-OTHER", "status", "serialNumber")
			}
			_, allReady, err = observeSelectedEnrollment(tenantStageContext(t), fake, execution, selected, initial, journal)
			if allReady || change != "lose-readiness" && err == nil {
				t.Fatalf("cohort drift accepted: ready=%t error=%v", allReady, err)
			}
			if change == "lose-readiness" && !strings.Contains(journal.file.(*managementMemoryFile).String(), "Ready condition is False") {
				t.Fatal("pending readiness reason was not journaled")
			}
		})
	}
}

func TestEnrollmentStageRejectsFinalDeviceReplacement(t *testing.T) {
	execution, fake, br, tr, password := enrollmentStageFixture(t)
	fake.bmcHook = func(n int) error {
		if n == 3 {
			fake.base.resources["DPUDevice/"+execution.operatorNamespace+"/serial-a"].SetUID("replacement")
		}
		return nil
	}
	receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution, br, tr, fake.selected, password, testJournal(execution), time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "ENROLLED_IDENTITY_CHANGED") || receipt.Secret.UID == "" {
		t.Fatalf("final drift result=%+v err=%v", receipt, err)
	}
}

func TestControlPlaneRejectsFactoryResetBeforeAnyManagementAccess(t *testing.T) {
	input := preflightGenerationInputs(t)
	input.Operator.Bootstrap.BMCFactoryResetPolicy = bootstrap.BMCFactoryResetPolicyOnInitialization
	generated, err := generation.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	journal := testJournal(execution)
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error { t.Fatal("reset policy reached probe effects"); return nil }}
	client := &APIClient{expected: execution.plan.Intent().Management, version: execution.plan.Intent().KubernetesVersion}
	_, err = runControlPlane(tenantStageContext(t), client, generated, journal, probes, controlPlaneTestRequest(t), time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "RESET_POLICY_UNSUPPORTED") {
		t.Fatalf("got %v", err)
	}
	if journal.file.(*managementMemoryFile).Len() != 0 {
		t.Fatal("reset policy failed after effects started")
	}
}

func TestEnrollmentStageReportsUnrecoverablePartialScan(t *testing.T) {
	execution, fake, br, tr, password := enrollmentStageFixture(t)
	fake.scanHook = func(_ *unstructured.Unstructured) {
		delete(fake.base.resources, "DPUNode/"+execution.operatorNamespace+"/dpu-node-serial-a")
	}
	journal := testJournal(execution)
	receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution, br, tr, fake.selected, password, journal, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "rescans cannot repair") || receipt.Secret.UID == "" {
		t.Fatalf("partial scan result=%+v err=%v", receipt, err)
	}
	if !strings.Contains(journal.file.(*managementMemoryFile).String(), "matching DPUNode") || fake.base.createCount != 2 {
		t.Fatal("partial scan was not explained or triggered an automatic repair")
	}
}

func TestEnrollmentScanReportsPartialCreationWithoutSuccessfulScanStatus(t *testing.T) {
	execution, fake, _, _, _ := enrollmentStageFixture(t)
	desired := retainedEnrollmentDiscoveries(execution)
	var scan *unstructured.Unstructured
	for _, obj := range desired {
		scan = obj
		break
	}
	marked, err := execution.reconciler.markedDesired(scan)
	if err != nil {
		t.Fatal(err)
	}
	marked.SetUID("scan-uid")
	marked.SetResourceVersion("1")
	marked.SetGeneration(1)
	fake.base.resources[tenantStageKey(marked)] = marked
	device := readyEnrollmentObjects(fake.selected[0], execution.operatorNamespace)[0]
	fake.base.resources[tenantStageKey(device)] = device
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	file := &cancelPendingEnrollmentFile{cancel: cancel, reason: "ENROLLMENT_NODE_CREATION_PENDING"}
	journal := &executionJournal{file: file, digest: execution.plan.Digest()}
	err = waitEnrollmentDiscovery(ctx, fake, execution, fake.selected, scan, marked.GetUID(), 5*time.Millisecond, journal)
	if err == nil || !strings.Contains(err.Error(), "ENROLLMENT_PARTIAL_NODE_MISSING") || fake.base.createCount != 0 {
		t.Fatalf("partial scan error=%v creates=%d", err, fake.base.createCount)
	}
	if !strings.Contains(file.String(), "ENROLLMENT_NODE_CREATION_PENDING") {
		t.Fatal("partial creation diagnostic missing before timeout")
	}
}

// Cancel only after the relevant pending observation was durably recorded;
// this exercises the timeout path without depending on fixture/CI timing.
type cancelPendingEnrollmentFile struct {
	managementMemoryFile
	cancel context.CancelFunc
	reason string
}

func (f *cancelPendingEnrollmentFile) Sync() error {
	if strings.Contains(f.String(), f.reason) {
		f.cancel()
	}
	return nil
}

func TestEnrollmentStageReusesExactDevicesAfterZeroNewDevicesScan(t *testing.T) {
	execution, fake, br, tr, password := enrollmentStageFixture(t)
	for _, obj := range readyEnrollmentObjects(fake.selected[0], execution.operatorNamespace) {
		fake.base.resources[tenantStageKey(obj)] = obj
	}
	fake.scanHook = func(scan *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(scan.Object, int64(0), "status", "foundDPUs")
	}
	receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution, br, tr, fake.selected, password, testJournal(execution), time.Millisecond, time.Second)
	if err != nil || len(receipt.Devices) != 1 || receipt.Devices[0].DeviceUID != "device-serial-a" || fake.base.createCount != 2 {
		t.Fatalf("exact reuse result=%+v creates=%d err=%v", receipt, fake.base.createCount, err)
	}
}

func TestEnrollmentZeroFoundScanCannotReplaceMissingSelectedDevice(t *testing.T) {
	execution, fake, br, tr, password := enrollmentStageFixture(t)
	fake.scanHook = func(scan *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(scan.Object, int64(0), "status", "foundDPUs")
		delete(fake.base.resources, "DPUDevice/"+execution.operatorNamespace+"/serial-a")
		delete(fake.base.resources, "DPUNode/"+execution.operatorNamespace+"/dpu-node-serial-a")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	file := &cancelPendingEnrollmentFile{cancel: cancel, reason: "ENROLLMENT_RESOURCE_PENDING"}
	journal := &executionJournal{file: file, digest: execution.plan.Digest()}
	receipt, err := runEnrollmentStage(ctx, fake, execution, br, tr, fake.selected, password, journal, time.Millisecond, time.Second)
	if err == nil || receipt.Secret.UID == "" || !strings.Contains(file.String(), "ENROLLMENT_RESOURCE_PENDING") {
		t.Fatalf("missing selected device accepted: %+v %v", receipt, err)
	}
	for _, device := range receipt.Devices {
		if device.DeviceUID != "" {
			t.Fatal("missing selected device returned a UID")
		}
	}
}

func TestEnrollmentStageChecksTenantAfterFinalDeviceReads(t *testing.T) {
	for _, change := range []string{"replace-tenant", "cancel"} {
		t.Run(change, func(t *testing.T) {
			execution, fake, br, tr, password := enrollmentStageFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			nodeReads := 0
			fake.getHook = func(wanted *unstructured.Unstructured) {
				if wanted.GetKind() == "DPUNode" {
					nodeReads++
					if nodeReads == 2 {
						if change == "replace-tenant" {
							fake.sources.sources.secret.UID = "replacement"
						} else {
							cancel()
						}
					}
				}
			}
			receipt, err := runEnrollmentStage(ctx, fake, execution, br, tr, fake.selected, password, testJournal(execution), time.Millisecond, time.Second)
			if err == nil || nodeReads != 2 || receipt.Secret.UID == "" {
				t.Fatalf("final read drift accepted: nodeReads=%d result=%+v err=%v", nodeReads, receipt, err)
			}
			if change == "replace-tenant" && !strings.Contains(err.Error(), "TENANT_API_IDENTITY_CHANGED") {
				t.Fatalf("tenant replacement reason missing: %v", err)
			}
			if change == "cancel" && !strings.Contains(err.Error(), "ENROLLMENT_READINESS_CANCELED") {
				t.Fatalf("cancellation reason missing: %v", err)
			}
		})
	}
}
