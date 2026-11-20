// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// This runtime substitute has no embedded executor, concrete API client, store,
// resetter or host configurer. Adding a writer to the port breaks compilation.
type runtimeReadOnlyFake struct {
	get      func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	list     func(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
	identity func(context.Context) error
	discover func(context.Context, string) (*metav1.APIResourceList, error)
	crd      func(context.Context, string) (*unstructured.Unstructured, error)
	registry func(context.Context, string) (registrySnapshot, error)
	physical func(context.Context, []enrollmentBMC, time.Duration, *executionJournal) error
}

func (f *runtimeReadOnlyFake) Get(c context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return f.get(c, o)
}
func (f *runtimeReadOnlyFake) List(c context.Context, o *unstructured.Unstructured, s string) ([]unstructured.Unstructured, error) {
	return f.list(c, o, s)
}
func (f *runtimeReadOnlyFake) VerifyIdentity(c context.Context) error { return f.identity(c) }
func (f *runtimeReadOnlyFake) discoverResourceList(c context.Context, s string) (*metav1.APIResourceList, error) {
	return f.discover(c, s)
}
func (f *runtimeReadOnlyFake) getOperatorCRD(c context.Context, s string) (*unstructured.Unstructured, error) {
	return f.crd(c, s)
}
func (f *runtimeReadOnlyFake) observeRegistrySnapshot(c context.Context, s string) (registrySnapshot, error) {
	return f.registry(c, s)
}
func (f *runtimeReadOnlyFake) verifyEnrollmentBMCs(c context.Context, s []enrollmentBMC, d time.Duration, j *executionJournal) error {
	return f.physical(c, s, d, j)
}

type runtimeTenantReadOnlyFake struct {
	identity        func(context.Context) error
	binding         func(tenantSourceReader, controllercontract.TenantReadinessExpected, types.UID) bool
	target          func(controllercontract.TenantReadinessExpected, types.UID) bool
	cluster         func() types.UID
	expected        func() controllercontract.TenantReadinessExpected
	child           func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	objects         func(context.Context, string, string) ([]appsv1.DaemonSet, []corev1.Pod, error)
	pod             func(context.Context, string, string) (*corev1.Pod, error)
	readPermissions func(context.Context, string) []PreflightCheck
	execPermissions func(context.Context, string, string) []PreflightCheck
}

func (f *runtimeTenantReadOnlyFake) VerifyIdentity(c context.Context) error { return f.identity(c) }
func (f *runtimeTenantReadOnlyFake) matchesTenantBinding(s tenantSourceReader, e controllercontract.TenantReadinessExpected, u types.UID) bool {
	return f.binding(s, e, u)
}
func (f *runtimeTenantReadOnlyFake) matchesTenantTarget(e controllercontract.TenantReadinessExpected, u types.UID) bool {
	return f.target(e, u)
}
func (f *runtimeTenantReadOnlyFake) clusterIdentity() types.UID { return f.cluster() }
func (f *runtimeTenantReadOnlyFake) expectation() controllercontract.TenantReadinessExpected {
	return f.expected()
}
func (f *runtimeTenantReadOnlyFake) getWorkloadChild(c context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return f.child(c, o)
}
func (f *runtimeTenantReadOnlyFake) listRuntimeObjects(c context.Context, n, s string) ([]appsv1.DaemonSet, []corev1.Pod, error) {
	return f.objects(c, n, s)
}
func (f *runtimeTenantReadOnlyFake) readPod(c context.Context, n, s string) (*corev1.Pod, error) {
	return f.pod(c, n, s)
}
func (f *runtimeTenantReadOnlyFake) checkTenantRuntimeReadPermissions(c context.Context, n string) []PreflightCheck {
	return f.readPermissions(c, n)
}
func (f *runtimeTenantReadOnlyFake) checkTenantPodExecPermissions(c context.Context, n, s string) []PreflightCheck {
	return f.execPermissions(c, n, s)
}

var _ activationBoundaryReader = (*runtimeReadOnlyFake)(nil)
var _ tenantVerificationReader = (*runtimeTenantReadOnlyFake)(nil)
var _ managementRunnerClient = (*APIClient)(nil)
var _ tenantStageManagementClient = (*APIClient)(nil)
var _ enrollmentManagementClient = (*APIClient)(nil)
var _ softwareStageManagementClient = (*APIClient)(nil)
var _ workloadStageClient = (*APIClient)(nil)
var _ activationStageClient = (*APIClient)(nil)
var _ activationBoundaryReader = (*APIClient)(nil)
var _ managementLeaseReader = (*APIClient)(nil)

func TestRuntimeReadOnlyPortsExerciseCompleteVerifierGraph(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	c := f.client
	reader := &runtimeReadOnlyFake{get: c.Get, list: c.List, identity: c.VerifyIdentity, discover: c.discoverResourceList, crd: c.getOperatorCRD, registry: c.observeRegistrySnapshot, physical: c.verifyEnrollmentBMCs}
	tenant := f.controlPlane.tenant.Client
	tenantReader := &runtimeTenantReadOnlyFake{identity: tenant.VerifyIdentity, binding: tenant.matchesTenantBinding, target: tenant.matchesTenantTarget, cluster: tenant.clusterIdentity, expected: tenant.expectation, child: tenant.getWorkloadChild, objects: tenant.listRuntimeObjects, pod: tenant.readPod, readPermissions: tenant.checkTenantRuntimeReadPermissions, execPermissions: tenant.checkTenantPodExecPermissions}
	fences, err := newDeploymentAcceptanceFences(testAcceptanceReceiptSource{reader}, newControlPlaneVerificationInputs(f.prepared, f.controlPlane, f.workload), tenantReader, f.activation, f.hosts, testJournal(f.prepared.execution), time.Second, fixtureAcceptanceLeases(f.transports.readLeases))
	if err != nil {
		t.Fatal(err)
	}
	if err := fences.physical(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := fences.receipts(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.commandReads != 0 || f.trafficReads != 0 {
		t.Fatal("receipt port measured runtime")
	}
	tenantPort := reflect.TypeFor[tenantVerificationReader]()
	for _, name := range []string{"readSecret", "readConfigMap", "readNode", "bootstrapCredentialSource", "operationSources"} {
		if _, found := tenantPort.MethodByName(name); found {
			t.Fatalf("runtime tenant received %s", name)
		}
	}
	port := reflect.TypeFor[activationBoundaryReader]()
	if port.NumMethod() != 7 {
		t.Fatalf("runtime management capability grew: %d methods", port.NumMethod())
	}
	for _, name := range []string{"Create", "ensureOperatorPlan", "patchEnrollmentSelectors", "patchActivationAnnotation", "openSoftwareRegistry", "executeEnrollmentSecret", "verifyActivationRebootBMCs"} {
		if _, found := port.MethodByName(name); found {
			t.Fatalf("runtime received %s", name)
		}
	}
}

func TestRuntimeVerificationViewsDetachMutableInputs(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	ownerCopy := *f.prepared.execution
	originalKamaji := f.prepared.execution.kamajiNamespace
	ownerCopy.kamajiNamespace = "changed-copy"
	if f.prepared.execution.kamajiNamespace != originalKamaji {
		t.Fatal("execution value copy aliases scalar projection")
	}
	first := newControlPlaneVerificationInputs(f.prepared, f.controlPlane, f.workload)
	second := newControlPlaneVerificationInputs(f.prepared, f.controlPlane, f.workload)
	activation, err := copyActivationVerificationReceipt(f.activation)
	if err != nil {
		t.Fatal(err)
	}
	key := "snapshot-test"
	f.prepared.selected[0].device.SelectorLabels[key] = "changed"
	f.controlPlane.management.NamespaceUIDs[key] = "changed"
	f.controlPlane.enrollment.Devices[0].DeviceUID = "changed"
	f.workload.Software.Components[0].SHA256 = "changed"
	f.workload.Resources[0].UID = "changed"
	f.prepared.execution.configObject.SetUID("changed")
	f.prepared.execution.manager.SetUID("changed")
	f.prepared.execution.crds[0].versions[0] = "changed"
	f.prepared.execution.crds[0].spec[key] = "changed"
	f.prepared.beforeDeploymentCreate = func(context.Context) error { t.Fatal("verification obtained create callback"); return nil }
	first.selected[0].device.SelectorLabels[key] = "first"
	first.management.NamespaceUIDs[key] = "first"
	if second.selected[0].device.SelectorLabels[key] != "" || second.management.NamespaceUIDs[key] != "" || second.enrollment.Devices[0].DeviceUID == "changed" || second.software.Components[0].SHA256 == "changed" || second.resources[0].UID == "changed" || second.execution.configObject.GetUID() == "changed" || second.execution.manager.GetUID() == "changed" || second.execution.crds[0].versions[0] == "changed" || second.execution.crds[0].spec[key] != nil {
		t.Fatal("verification input shares mutable state")
	}
	for host, indexes := range f.activation.groups {
		indexes[0] = 99
		if activation.groups[host][0] == 99 {
			t.Fatal("activation group shares indexes")
		}
		break
	}
	for node, progress := range f.activation.progress {
		progress.sets[key] = "changed"
		if activation.progress[node].sets[key] != "" {
			t.Fatal("activation progress shares bindings")
		}
		break
	}
	f.activation.selected[0].host.Adapters[0].PCIFunctions[0].Netdevs[0] = "changed"
	f.activation.selected[0].dpuSet.SetUID("changed")
	if activation.selected[0].host.Adapters[0].PCIFunctions[0].Netdevs[0] == "changed" || activation.selected[0].dpuSet.GetUID() == "changed" {
		t.Fatal("activation selected data shares state")
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[controlPlaneVerificationInputs](), reflect.TypeFor[managementVerificationInputs]()} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Type.Kind() == reflect.Func || field.Type == reflect.TypeFor[*tenantClient]() || field.Type == reflect.TypeFor[*deploymentOperation]() || field.Type == reflect.TypeFor[*operatorPlan]() {
				t.Fatalf("verification input retains effect handle %s.%s", typ.Name(), field.Name)
			}
		}
	}
}

func TestActivationVerificationCopyPreservesTypedFactPresence(t *testing.T) {
	enabled, group := false, 0
	input := activationStageReceipt{selected: []activationSelectedDevice{{
		host: inventory.Device{Firmware: []inventory.Firmware{}, Adapters: nil, Observations: map[string]inventory.Observation{},
			Time:    inventory.TimeObservation{NTPEnabled: &enabled, NTPServers: []string{}},
			Runtime: &inventory.RuntimeObservation{VirtualMachine: &enabled, Interfaces: []inventory.Interface{}, Routes: nil}},
		device:  inventory.Device{IdentityKey: []string{}, Firmware: nil},
		adapter: inventory.Adapter{PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0", IOMMUGroup: &group, Netdevs: []string{}}}},
	}}}
	copied, err := copyActivationVerificationReceipt(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, copied) {
		t.Fatal("typed fact copy changed nil, empty or pointer presence")
	}
	*copied.selected[0].host.Time.NTPEnabled = true
	*copied.selected[0].host.Runtime.VirtualMachine = true
	*copied.selected[0].adapter.PCIFunctions[0].IOMMUGroup = 7
	copied.selected[0].host.Observations["copy"] = inventory.Observation{State: "observed"}
	if *input.selected[0].host.Time.NTPEnabled || *input.selected[0].host.Runtime.VirtualMachine || *input.selected[0].adapter.PCIFunctions[0].IOMMUGroup != 0 || len(input.selected[0].host.Observations) != 0 {
		t.Fatal("typed fact copy aliases mutable pointers or maps")
	}
}
