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
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

type softwarePreparationFake struct {
	*softwareStageFake
	tenant                     *tenantClient
	callerState                string
	createKinds                []string
	replaceTemplateOnInterface bool
}

func (f *softwarePreparationFake) checkCallerPermissions(context.Context, preflightPlan) PreflightResult {
	state := f.callerState
	if state == "" {
		state = "passed"
	}
	return PreflightResult{Ready: state == "passed", Checks: []PreflightCheck{{ID: "caller.workload", State: state, ReasonCode: "CALLER_WORKLOAD_PERMISSION_CHECKED", Message: "fixture permission result"}}}
}

func (f *softwarePreparationFake) Create(ctx context.Context, desired *unstructured.Unstructured, dry bool) (*unstructured.Unstructured, error) {
	f.createKinds = append(f.createKinds, desired.GetKind())
	if f.replaceTemplateOnInterface && desired.GetKind() == "DPUServiceInterface" {
		for _, template := range f.base.resources {
			if template.GetKind() == "DPUServiceTemplate" && template.GetNamespace() == desired.GetNamespace() {
				template.SetUID("replacement-template-uid")
				f.replaceTemplateOnInterface = false
				break
			}
		}
	}
	created, err := f.softwareStageFake.Create(ctx, desired, dry)
	if err != nil || desired.GetKind() == "BlueFieldSoftware" || desired.GetKind() == "DPUDiscovery" || desired.GetKind() == "Secret" {
		return created, err
	}
	created.SetUID(types.UID("workload-" + strings.ToLower(created.GetKind()) + "-" + created.GetName()))
	created.SetResourceVersion("1")
	created.SetGeneration(1)
	switch created.GetKind() {
	case "DPUServiceTemplate":
		setWorkloadStatus(created, "Ready", "DPUServiceTemplateReconciled")
	case "DPUServiceIPAM":
		setWorkloadStatus(created, "DPUIPAMObjectReconciled")
		tenant := f.tenant.expected
		_ = unstructured.SetNestedSlice(created.Object, []any{map[string]any{"dpuCluster": tenant.Namespace + "/" + tenant.Name, "ipRanges": []any{}}}, "status", "dpuClusterAllocations")
	case "DPUServiceInterface":
		setWorkloadStatus(created, "ServiceInterfaceSetReconciled")
	}
	f.base.resources[tenantStageKey(created)] = created.DeepCopy()
	return created.DeepCopy(), nil
}

func setWorkloadStatus(object *unstructured.Unstructured, conditions ...string) {
	values := make([]any, 0, len(conditions))
	for _, condition := range conditions {
		values = append(values, conditionMap(condition, "True", "Success", 1))
	}
	object.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": values}
}

func (f *softwarePreparationFake) List(ctx context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	switch wanted.GetKind() {
	case "DPUCluster":
		return f.base.List(ctx, wanted, selector)
	case "DPFOperatorConfig":
		return f.operator.List(ctx, wanted, selector)
	default:
		return f.enrollmentStageFake.List(ctx, wanted, selector)
	}
}

func (f *softwarePreparationFake) getTenantWorkloadChild(_ context.Context, tenant *tenantClient, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if tenant == nil || parent == nil {
		return nil, fmt.Errorf("tenant and parent required")
	}
	switch parent.GetKind() {
	case "DPUServiceInterface":
		spec, found, err := unstructured.NestedMap(parent.Object, "spec", "template", "spec")
		if err != nil || !found {
			return nil, fmt.Errorf("interface template spec missing")
		}
		labels, _, _ := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "labels")
		annotations, _, _ := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "annotations")
		return workloadChildObject("svc.dpu.nvidia.com/v1alpha1", "ServiceInterfaceSet", parent.GetName(), parent.GetNamespace(), map[string]any{"spec": runtime.DeepCopyJSONValue(spec)}, labels, annotations), nil
	case "DPUServiceIPAM":
		network, found, err := unstructured.NestedMap(parent.Object, "spec", "ipv4Network")
		if err != nil || !found {
			return nil, fmt.Errorf("IPAM network missing")
		}
		poolSpec := map[string]any{"cidr": network["network"], "perNodeNetworkPrefix": network["prefixSize"]}
		for _, key := range []string{"gatewayIndex", "excludeRanges", "defaultGateway", "routes"} {
			if value, present := network[key]; present {
				poolKey := key
				if key == "excludeRanges" {
					poolKey = "exclusions"
				}
				poolSpec[poolKey] = runtime.DeepCopyJSONValue(value)
			}
		}
		if selector, found, _ := unstructured.NestedMap(parent.Object, "spec", "nodeSelector"); found {
			poolSpec["nodeSelector"] = runtime.DeepCopyJSONValue(selector)
		}
		labels, _, _ := unstructured.NestedStringMap(parent.Object, "spec", "labels")
		labels = cloneTestStringMap(labels)
		labels["dpu.nvidia.com/dpuserviceipam-name"] = parent.GetName()
		labels["dpu.nvidia.com/dpuserviceipam-namespace"] = parent.GetNamespace()
		return workloadChildObject("nv-ipam.nvidia.com/v1alpha1", "CIDRPool", parent.GetName(), parent.GetNamespace(), map[string]any{"spec": poolSpec}, labels, nil), nil
	default:
		return nil, fmt.Errorf("unsupported tenant child kind")
	}
}

func cloneTestStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func workloadChildObject(apiVersion, kind, name, namespace string, object map[string]any, labels, annotations map[string]string) *unstructured.Unstructured {
	result := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{
		"name": name, "namespace": namespace, "uid": "child-" + name, "resourceVersion": "1", "labels": stringMapObject(labels), "annotations": stringMapObject(annotations),
	}}}
	for key, value := range object {
		result.Object[key] = value
	}
	return result
}

func stringMapObject(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (f *softwarePreparationFake) checkManagementPreflight(ctx context.Context, plan preflightPlan) PreflightResult {
	return f.operator.checkManagementPreflight(ctx, plan)
}

func (f *softwarePreparationFake) checkDPFOperatorConfigPermissions(ctx context.Context, namespace string) []PreflightCheck {
	return f.operator.checkDPFOperatorConfigPermissions(ctx, namespace)
}

func (f *softwarePreparationFake) discoverResourceList(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	return f.operator.discoverResourceList(ctx, groupVersion)
}

func (f *softwarePreparationFake) ensureOperatorPlan(ctx context.Context, plan *operatorPlan) error {
	return f.operator.ensureOperatorPlan(ctx, plan)
}

func (f *softwarePreparationFake) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if wanted.GetKind() == "CustomResourceDefinition" {
		return f.operator.Get(ctx, wanted)
	}
	return f.softwareStageFake.Get(ctx, wanted)
}

func (f *softwarePreparationFake) checkTenantStorage(ctx context.Context, namespace string) PreflightCheck {
	return f.base.checkTenantStorage(ctx, namespace)
}

func (f *softwarePreparationFake) VerifyIdentity(ctx context.Context) error {
	return f.softwareStageFake.VerifyIdentity(ctx)
}

func (f *softwarePreparationFake) checkTenantNetwork(_ context.Context, _ bootstrap.Tenant, _ tenantNetworkRunner, _ string, _ time.Duration) ([]inventory.Check, error) {
	return []inventory.Check{{ID: "test.network", State: "passed", ReasonCode: "NETWORK_FIXTURE", Message: "fixture observation"}}, nil
}

func (f *softwarePreparationFake) connectTenant(ctx context.Context, _ *generation.Plan, uid types.UID, _ *Reconciler, _ *executionJournal, _, _ time.Duration) (*tenantClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.tenant.dpuClusterUID = string(uid)
	return f.tenant, nil
}

func TestRunSoftwarePreparationPassesOneRetainedBundleThroughStages(t *testing.T) {
	execution, _, selected, _, stageFake, _ := softwareStageFixture(t)
	retained := execution.retained
	base := stageFake.base
	operator := stageFake.operator
	prepared := &softwarePreparationFake{softwareStageFake: stageFake}

	// Seed an exact, controller-ready DPUCluster and tenant source matching the
	// retained plan. This tests stage composition and plan handoff, not a clean
	// cluster's controller installation lifecycle.
	expected, desired, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	current, err := execution.reconciler.markedDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID(types.UID("dc-uid"))
	current.SetResourceVersion("5")
	current.Object["status"] = runtime.DeepCopyJSONValue(stageFake.sources.sources.observed.DPUCluster.Object["status"])
	base.resources[tenantStageKey(current)] = current.DeepCopy()
	base.clusters = []unstructured.Unstructured{*current.DeepCopy()}
	if err := operatorBindingForExecution(operator, execution); err != nil {
		t.Fatal(err)
	}
	tenant, _ := tenantIdentityClientFixture(t, expected, stageFake.sources)
	tenant.source = prepared
	prepared.tenant = tenant
	base.tenant = tenant

	request := controlPlaneTestRequest(t)
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://192.0.2.30"
		}
	}
	journal := testJournal(execution)
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error { return nil }}
	ctx := tenantStageContext(t)
	got, err := runSoftwarePreparation(ctx, prepared, retained, journal, probes, request, time.Millisecond, time.Second, 1<<20)
	if err != nil {
		t.Fatalf("runSoftwarePreparation() error = %v", err)
	}
	if got.BundleDigest != execution.plan.Digest() || got.ControlPlane.enrollment.BundleDigest != got.BundleDigest || len(got.ControlPlane.enrollment.Devices) != len(selected) || len(got.Software.Components) != 2 {
		t.Fatalf("preparation receipt does not preserve the selected plan and completed stage receipts: %#v", got)
	}
	if stageFake.createSoftware != 1 || stageFake.streamRequests.Load() != 2 || stageFake.tunnelCloses != 1 {
		t.Fatalf("software effect counts create=%d streams=%d close=%d", stageFake.createSoftware, stageFake.streamRequests.Load(), stageFake.tunnelCloses)
	}
}

func TestRunWorkloadPreparationStopsBeforeDPUDeployment(t *testing.T) {
	execution, selected, prepared, request, journal, probeRunner := workloadPreparationFixture(t)
	objects, err := retainedWorkloadObjects(execution)
	if err != nil {
		t.Fatal(err)
	}
	sawHostReservations := false
	for _, object := range objects {
		if object.GetKind() != "DPUServiceIPAM" && object.GetKind() != "DPUServiceInterface" {
			continue
		}
		if object.GetKind() == "DPUServiceIPAM" {
			ranges, found, rangeErr := unstructured.NestedSlice(object.Object, "spec", "ipv4Network", "excludeRanges")
			if rangeErr != nil {
				t.Fatal(rangeErr)
			}
			sawHostReservations = sawHostReservations || found && len(ranges) > 0
		}
		child, err := prepared.getTenantWorkloadChild(context.Background(), prepared.tenant, object)
		if err != nil {
			t.Fatal(err)
		}
		parent := object.DeepCopy()
		if parent.GetKind() == "DPUServiceIPAM" {
			_ = unstructured.SetNestedSlice(parent.Object, []any{map[string]any{"dpuCluster": prepared.tenant.expected.Namespace + "/" + prepared.tenant.expected.Name, "ipRanges": []any{}}}, "status", "dpuClusterAllocations")
		}
		if err := verifyWorkloadChild(parent, child, prepared.tenant.expected); err != nil {
			t.Fatalf("rendered %s child mapping is not represented by pinned controller transform: %v parent=%#v child=%#v labels=%#v", object.GetKind(), err, object.Object["spec"], child.Object["spec"], child.GetLabels())
		}
	}
	if !sawHostReservations {
		t.Fatal("retained HBN fixture did not exercise rendered host-reservation propagation")
	}

	var sourceChecks int
	verifySource := func(_ context.Context, plan *generation.Plan) (PreflightCheck, error) {
		sourceChecks++
		if plan.Digest() != execution.plan.Digest() {
			return PreflightCheck{}, fmt.Errorf("chart verifier received a different plan")
		}
		return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "fixture archive check"}, nil
	}
	got, err := runWorkloadPreparation(tenantStageContext(t), prepared, execution.retained, journal, probeRunner, request,
		time.Millisecond, time.Second, 1<<20, verifySource)
	if err != nil {
		t.Fatalf("runWorkloadPreparation() error = %v", err)
	}
	if got.BundleDigest != execution.plan.Digest() || got.ControlPlane.enrollment.BundleDigest != got.BundleDigest || len(got.ControlPlane.enrollment.Devices) != len(selected) ||
		got.Software.SoftwareUID == "" || len(got.Resources) != 8 || sourceChecks != 2 {
		t.Fatalf("workload receipt does not bind the one retained plan and completed prerequisites: %#v source checks=%d", got, sourceChecks)
	}
	for _, kind := range prepared.createKinds {
		if kind == "DPUDeployment" {
			t.Fatal("preparation stage created DPUDeployment")
		}
	}
	for _, kind := range []string{"BlueFieldSoftware", "DPUFlavor", "DPUServiceConfiguration", "DPUServiceTemplate", "DPUServiceIPAM", "DPUServiceInterface"} {
		found := false
		for _, gotKind := range prepared.createKinds {
			if gotKind == kind {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("preparation did not create or verify expected kind %s: %v", kind, prepared.createKinds)
		}
	}
	if !strings.Contains(journal.file.(*managementMemoryFile).String(), "WORKLOAD_PREREQUISITES_PREPARED") {
		t.Fatal("prepared receipt was not durably journaled")
	}
}

func TestRunWorkloadPreparationSourceOrPermissionFailurePreventsEffects(t *testing.T) {
	for _, test := range []struct {
		name       string
		deny       bool
		sourceFail bool
	}{
		{name: "chart source mismatch", sourceFail: true},
		{name: "caller permission denied", deny: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution, _, prepared, request, journal, probeRunner := workloadPreparationFixture(t)
			if test.deny {
				prepared.callerState = "failed"
			}
			_, err := runWorkloadPreparation(tenantStageContext(t), prepared, execution.retained, journal, probeRunner, request,
				time.Millisecond, time.Second, 1<<20, func(context.Context, *generation.Plan) (PreflightCheck, error) {
					if test.sourceFail {
						return PreflightCheck{}, fmt.Errorf("source mismatch")
					}
					return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "fixture archive check"}, nil
				})
			if err == nil {
				t.Fatal("blocked source or permission check returned success")
			}
			for _, kind := range prepared.createKinds {
				if kind == "DPUDeployment" || kind == "DPUFlavor" || kind == "DPUServiceConfiguration" || kind == "DPUServiceTemplate" || kind == "DPUServiceIPAM" || kind == "DPUServiceInterface" || kind == "BlueFieldSoftware" {
					t.Fatalf("preflight failure created a workload resource: %v", prepared.createKinds)
				}
			}
			if test.sourceFail && strings.Contains(journal.file.(*managementMemoryFile).String(), "WORKLOAD_PREREQUISITES_PREPARED") {
				t.Fatal("source failure was recorded as a successful preparation")
			}
		})
	}
}

func TestRunWorkloadPreparationStopsOnReplacementBeforeLaterResources(t *testing.T) {
	execution, _, prepared, request, journal, probes := workloadPreparationFixture(t)
	replaced := false
	prepared.getHook = func(wanted *unstructured.Unstructured) {
		if wanted.GetKind() != "DPUServiceTemplate" || replaced {
			return
		}
		if current := prepared.base.resources[tenantStageKey(wanted)]; current != nil {
			current.SetUID("replacement-template-uid")
			replaced = true
		}
	}
	_, err := runWorkloadPreparation(tenantStageContext(t), prepared, execution.retained, journal, probes, request,
		time.Millisecond, time.Second, 1<<20, func(context.Context, *generation.Plan) (PreflightCheck, error) {
			return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "fixture archive check"}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "WORKLOAD_RESOURCE_REPLACED") {
		t.Fatalf("replaced Template did not stop preparation with a specific reason: %v", err)
	}
	if !replaced {
		t.Fatal("test did not replace the created Template at its readiness boundary")
	}
	for _, kind := range prepared.createKinds {
		switch kind {
		case "DPUServiceIPAM", "DPUServiceInterface", "DPUDeployment":
			t.Fatalf("a later workload effect followed Template replacement: %v", prepared.createKinds)
		}
	}
	if strings.Contains(journal.file.(*managementMemoryFile).String(), "WORKLOAD_PREREQUISITES_PREPARED") {
		t.Fatal("replacement run recorded the workload stage as complete")
	}
}

func TestRunWorkloadPreparationRechecksEarlierResourcesBeforeReceipt(t *testing.T) {
	execution, _, prepared, request, journal, probes := workloadPreparationFixture(t)
	prepared.replaceTemplateOnInterface = true
	_, err := runWorkloadPreparation(tenantStageContext(t), prepared, execution.retained, journal, probes, request,
		time.Millisecond, time.Second, 1<<20, func(context.Context, *generation.Plan) (PreflightCheck, error) {
			return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "fixture archive check"}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "WORKLOAD_RESOURCE_SET_CHANGED") {
		t.Fatalf("changed earlier Template did not block the final preparation receipt: %v", err)
	}
	if strings.Contains(journal.file.(*managementMemoryFile).String(), "WORKLOAD_PREREQUISITES_PREPARED") {
		t.Fatal("changed workload resource set was reported prepared")
	}
}

func workloadPreparationFixture(t *testing.T) (*managementExecution, []enrollmentBMC, *softwarePreparationFake, config.Request, *executionJournal, probe.Runner) {
	t.Helper()
	execution, _, selected, _, stageFake, _ := softwareStageFixture(t)
	prepared := &softwarePreparationFake{softwareStageFake: stageFake}
	expected, desired, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	current, err := execution.reconciler.markedDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID(types.UID("dc-uid"))
	current.SetResourceVersion("5")
	current.SetGeneration(1)
	current.Object["status"] = runtime.DeepCopyJSONValue(stageFake.sources.sources.observed.DPUCluster.Object["status"])
	prepared.base.resources[tenantStageKey(current)] = current.DeepCopy()
	prepared.base.clusters = []unstructured.Unstructured{*current.DeepCopy()}
	if err := operatorBindingForExecution(prepared.operator, execution); err != nil {
		t.Fatal(err)
	}
	tenant, _ := tenantIdentityClientFixture(t, expected, stageFake.sources)
	tenant.source = prepared
	prepared.tenant = tenant
	prepared.base.tenant = tenant
	request := controlPlaneTestRequest(t)
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://192.0.2.30"
		}
	}
	journal := testJournal(execution)
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error { return nil }}
	return execution, selected, prepared, request, journal, probes
}

func operatorBindingForExecution(fake *managementFake, execution *managementExecution) error {
	if fake == nil || execution == nil || execution.plan == nil || fake.bundleDigest != execution.plan.Digest() {
		return fmt.Errorf("management fake is not bound to the retained plan")
	}
	fake.binding = true
	return nil
}

var _ softwareStageManagementClient = (*softwarePreparationFake)(nil)
var _ managementRunnerClient = (*softwarePreparationFake)(nil)
