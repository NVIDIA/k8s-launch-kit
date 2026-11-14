// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunTenantStageOrdersFreshChecksCreateAndTenantConnection(t *testing.T) {
	execution, fake, receipt := newTenantStageFixture(t)
	networkCalls := 0
	result, stageErr := runTenantStage(tenantStageContext(t), fake, execution, receipt, testJournal(execution), time.Millisecond, time.Second,
		func(_ context.Context, _ bootstrap.Tenant) ([]inventory.Check, error) {
			networkCalls++
			fake.events = append(fake.events, "network")
			return []inventory.Check{{ID: "network", Subject: "node-a", State: "passed", ReasonCode: "NETWORK_READY"}}, nil
		})
	if stageErr != nil {
		t.Fatalf("runTenantStage() error = %v", stageErr)
	}
	if result.ClusterUID != "cluster-created" || result.Client != fake.tenant || networkCalls != 1 || fake.connectUID != "cluster-created" {
		t.Fatalf("tenant receipt=%#v networkCalls=%d connectUID=%q", result, networkCalls, fake.connectUID)
	}
	if fake.createCount != 1 || fake.listCount != 2 || fake.storageCount != 2 {
		t.Fatalf("create/list/storage counts = %d/%d/%d", fake.createCount, fake.listCount, fake.storageCount)
	}
	if len(fake.storageNamespaces) != 2 || fake.storageNamespaces[0] != execution.kamajiNamespace || fake.storageNamespaces[1] != execution.kamajiNamespace {
		t.Fatalf("stage storage checks used namespaces %v, want prepared Kamaji namespace %q", fake.storageNamespaces, execution.kamajiNamespace)
	}
	requireOrderedTenantEvents(t, fake.events, "storage", "list", "network", "list", "storage", "create:DPUCluster", "connect")
}

func TestRunTenantStageRejectsInvalidPreparedKamajiNamespaceBeforeManagementAccess(t *testing.T) {
	execution, fake, receipt := newTenantStageFixture(t)
	invalid := *execution
	invalid.kamajiNamespace = "bad namespace"
	_, err := runTenantStage(tenantStageContext(t), fake, &invalid, receipt, testJournal(execution), time.Millisecond, time.Second,
		func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
			t.Fatal("invalid namespace reached network probe")
			return nil, nil
		})
	if err == nil || len(fake.events) != 0 {
		t.Fatalf("invalid prepared namespace reached management API: events=%v err=%v", fake.events, err)
	}
}

func TestRunTenantStageExistingClusterSkipsVacancyProbeAndNeverRecreates(t *testing.T) {
	t.Run("exact existing target", func(t *testing.T) {
		execution, fake, receipt := newTenantStageFixture(t)
		desired := tenantDesiredObject(t, execution)
		marked, err := execution.reconciler.markedDesired(desired)
		if err != nil {
			t.Fatal(err)
		}
		marked.SetUID(types.UID("cluster-existing"))
		marked.SetResourceVersion("2")
		fake.resources[tenantStageKey(marked)] = marked
		fake.clusters = []unstructured.Unstructured{*marked.DeepCopy()}
		probeCalls := 0
		result, stageErr := runTenantStage(tenantStageContext(t), fake, execution, receipt, testJournal(execution), time.Millisecond, time.Second,
			func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
				probeCalls++
				return nil, fmt.Errorf("should not probe an existing VIP")
			})
		if stageErr != nil || result.ClusterUID != "cluster-existing" || probeCalls != 0 || fake.createCount != 0 || fake.connectUID != "cluster-existing" {
			t.Fatalf("existing target result=%#v probe=%d creates=%d connectUID=%q err=%v", result, probeCalls, fake.createCount, fake.connectUID, stageErr)
		}
	})

	t.Run("target disappears after list", func(t *testing.T) {
		execution, fake, receipt := newTenantStageFixture(t)
		desired := tenantDesiredObject(t, execution)
		marked, err := execution.reconciler.markedDesired(desired)
		if err != nil {
			t.Fatal(err)
		}
		marked.SetUID(types.UID("cluster-existing"))
		marked.SetResourceVersion("2")
		fake.clusters = []unstructured.Unstructured{*marked.DeepCopy()}
		fake.listHook = func(call int) {
			if call == 1 {
				fake.clusters = nil
			}
		}
		probeCalls := 0
		_, stageErr := runTenantStage(tenantStageContext(t), fake, execution, receipt, testJournal(execution), time.Millisecond, time.Second,
			func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) { probeCalls++; return nil, nil })
		if stageErr == nil || fake.createCount != 0 || fake.connectUID != "" || probeCalls != 0 {
			t.Fatalf("disappearing target was recreated or connected: creates=%d connectUID=%q probes=%d err=%v", fake.createCount, fake.connectUID, probeCalls, stageErr)
		}
	})
}

func TestRunTenantStageStopsBeforeCreateOnPrerequisiteFailure(t *testing.T) {
	tests := []struct {
		name          string
		configure     func(*tenantStageFake, *managementExecution)
		allocation    func(*generation.Inputs)
		wantNetwork   bool
		wantListCount int
	}{
		{name: "allocation unknown", allocation: func(in *generation.Inputs) { in.Operator.Bootstrap.Tenant.Allocation = nil }, wantListCount: 0},
		{name: "allocation rejected", allocation: func(in *generation.Inputs) {
			no := false
			in.Operator.Bootstrap.Tenant.Allocation.VIPReserved = &no
		}, wantListCount: 0},
		{name: "storage unavailable", configure: func(f *tenantStageFake, _ *managementExecution) {
			f.storage = []PreflightCheck{{ID: "storage", State: "blocked", ReasonCode: "STORAGE_UNKNOWN"}}
		}, wantListCount: 0},
		{name: "network unknown", configure: func(_ *tenantStageFake, _ *managementExecution) {}, wantNetwork: true, wantListCount: 1},
		{name: "foreign VRID collision", configure: func(f *tenantStageFake, execution *managementExecution) {
			other := tenantDesiredObject(t, execution)
			other.SetName("other-tenant")
			other.SetNamespace("other-namespace")
			other.SetUID(types.UID("other-uid"))
			_ = unstructured.SetNestedField(other.Object, "192.0.2.99", "spec", "clusterEndpoint", "keepalived", "vip")
			f.clusters = []unstructured.Unstructured{*other}
		}, wantListCount: 1},
		{name: "foreign VIP collision", configure: func(f *tenantStageFake, execution *managementExecution) {
			other := tenantDesiredObject(t, execution)
			other.SetName("other-tenant")
			other.SetNamespace("other-namespace")
			other.SetUID(types.UID("other-uid"))
			f.clusters = []unstructured.Unstructured{*other}
		}, wantListCount: 1},
		{name: "receipt manager changed", configure: func(_ *tenantStageFake, _ *managementExecution) {}, wantListCount: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := preflightGenerationInputs(t)
			setSiteAllocation(&input.Operator.Bootstrap.Tenant)
			if test.allocation != nil {
				test.allocation(&input)
			}
			generated, err := generation.Generate(input)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := prepareManagementExecution(generated, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			fake := newTenantStageFake(t, execution)
			if test.configure != nil {
				test.configure(fake, execution)
			}
			receipt := tenantStageBootstrapReceipt(execution, fake)
			if test.name == "receipt manager changed" {
				receipt.ManagerDeploymentUID = "replacement"
			}
			networkCalls := 0
			_, stageErr := runTenantStage(tenantStageContext(t), fake, execution, receipt, testJournal(execution), time.Millisecond, time.Second,
				func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
					networkCalls++
					if test.wantNetwork {
						return []inventory.Check{{ID: "network", Subject: "node-a", State: "unknown", ReasonCode: "NETWORK_UNAVAILABLE"}}, nil
					}
					return []inventory.Check{{ID: "network", Subject: "node-a", State: "passed", ReasonCode: "NETWORK_READY"}}, nil
				})
			if stageErr == nil || fake.createCount != 0 || fake.connectUID != "" || fake.listCount != test.wantListCount {
				t.Fatalf("prerequisite failure advanced: create=%d connect=%q list=%d err=%v", fake.createCount, fake.connectUID, fake.listCount, stageErr)
			}
			if (networkCalls > 0) != test.wantNetwork {
				t.Fatalf("network calls=%d wantNetwork=%v", networkCalls, test.wantNetwork)
			}
		})
	}
}

func TestTenantRegistryCollisionChecksVIPAndOverlappingVRID(t *testing.T) {
	execution, _, _ := newTenantStageFixture(t)
	desired := tenantDesiredObject(t, execution)
	other := desired.DeepCopy()
	other.SetName("other-tenant")
	other.SetNamespace("other-namespace")
	other.SetUID(types.UID("other-uid"))
	_ = unstructured.SetNestedField(other.Object, "192.0.2.99", "spec", "clusterEndpoint", "keepalived", "vip")
	if _, err := checkTenantRegistry([]unstructured.Unstructured{*other}, desired, "192.0.2.11"); err == nil || !strings.Contains(err.Error(), "virtual router ID") {
		t.Fatalf("overlapping VRID was not rejected: %v", err)
	}
	_ = unstructured.SetNestedStringMap(other.Object, map[string]string{"node-role.kubernetes.io/control-plane": "different"}, "spec", "clusterEndpoint", "keepalived", "nodeSelector")
	if _, err := checkTenantRegistry([]unstructured.Unstructured{*other}, desired, "192.0.2.11"); err != nil {
		t.Fatalf("disjoint node selector falsely conflicted: %v", err)
	}
	_ = unstructured.SetNestedField(other.Object, "192.0.2.11", "spec", "clusterEndpoint", "keepalived", "vip")
	if _, err := checkTenantRegistry([]unstructured.Unstructured{*other}, desired, "192.0.2.11"); err == nil || !strings.Contains(err.Error(), "VIP") {
		t.Fatalf("VIP collision was not rejected: %v", err)
	}
	static := other.DeepCopy()
	_ = unstructured.SetNestedField(static.Object, "static", "spec", "type")
	unstructured.RemoveNestedField(static.Object, "spec", "clusterEndpoint")
	if _, err := checkTenantRegistry([]unstructured.Unstructured{*static}, desired, "192.0.2.11"); err != nil {
		t.Fatalf("supported static DPUCluster without Keepalived blocked the registry scan: %v", err)
	}
}

func TestTenantStageJournalsPartialNetworkFailure(t *testing.T) {
	execution, fake, receipt := newTenantStageFixture(t)
	journal := testJournal(execution)
	_, err := runTenantStage(tenantStageContext(t), fake, execution, receipt, journal, time.Millisecond, time.Second,
		func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
			return []inventory.Check{{ID: "probe.node", Subject: "node-a", State: "unknown", ReasonCode: "TENANT_PROBE_CLEANUP_INCOMPLETE", Message: "Temporary probe cleanup is incomplete."}}, fmt.Errorf("private runner detail")
		})
	if err == nil || fake.createCount != 0 {
		t.Fatalf("partial probe failure advanced to create: create=%d err=%v", fake.createCount, err)
	}
	contents := journal.file.(*managementMemoryFile).String()
	if !strings.Contains(contents, "TENANT_PROBE_CLEANUP_INCOMPLETE") || !strings.Contains(contents, "Temporary probe cleanup is incomplete.") || strings.Contains(contents, "private runner detail") {
		t.Fatalf("journal lost safe probe reason or leaked raw runner error: %s", contents)
	}
}

func TestRunTenantStagePreservesMissingAllocationAsUnknown(t *testing.T) {
	input := preflightGenerationInputs(t)
	input.Operator.Bootstrap.Tenant.Allocation = nil
	generated, err := generation.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fake := newTenantStageFake(t, execution)
	journal := testJournal(execution)
	_, err = runTenantStage(tenantStageContext(t), fake, execution, tenantStageBootstrapReceipt(execution, fake), journal, time.Millisecond, time.Second,
		func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
			t.Fatal("missing allocation reached network probe")
			return nil, nil
		})
	if err == nil || len(fake.events) != 0 {
		t.Fatalf("missing allocation reached management API: events=%v err=%v", fake.events, err)
	}
	foundMissing := false
	for _, line := range strings.Split(journal.file.(*managementMemoryFile).String(), "\n") {
		if line == "" {
			continue
		}
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.ReasonCode == "TENANT_SITE_ALLOCATION_MISSING" {
			foundMissing = true
			if event.State != "unknown" {
				t.Fatalf("missing declaration was flattened to %q instead of unknown", event.State)
			}
		}
	}
	if !foundMissing {
		t.Fatal("missing allocation prerequisite was not preserved in the stage journal")
	}
}

type tenantStageFake struct {
	execution         *managementExecution
	resources         map[string]*unstructured.Unstructured
	clusters          []unstructured.Unstructured
	events            []string
	storage           []PreflightCheck
	storageNamespaces []string
	storageCount      int
	listCount         int
	createCount       int
	listHook          func(int)
	tenant            *tenantClient
	connectUID        types.UID
}

func newTenantStageFake(t *testing.T, execution *managementExecution) *tenantStageFake {
	t.Helper()
	fake := &tenantStageFake{execution: execution, resources: map[string]*unstructured.Unstructured{}, tenant: &tenantClient{},
		storage: []PreflightCheck{{ID: "external.kamaji.storage", State: "passed", ReasonCode: "STORAGE_READY", Message: "storage ready"}}}
	for i, namespace := range execution.namespaces {
		current := namespace.DeepCopy()
		current.SetUID(types.UID(fmt.Sprintf("namespace-%d", i)))
		current.SetResourceVersion("1")
		current.Object["status"] = map[string]any{"phase": "Active"}
		fake.resources[tenantStageKey(current)] = current
	}
	config, err := execution.reconciler.markedDesired(execution.configObject)
	if err != nil {
		t.Fatal(err)
	}
	config.SetUID(types.UID("config-uid"))
	config.SetResourceVersion("1")
	config.SetGeneration(1)
	config.Object["status"] = map[string]any{"observedGeneration": int64(1), "version": execution.operatorVersion, "targetVersion": execution.operatorVersion,
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}}
	fake.resources[tenantStageKey(config)] = config
	manager := execution.manager.DeepCopy()
	manager.SetUID(types.UID("manager-uid"))
	manager.SetResourceVersion("1")
	manager.SetGeneration(1)
	replicas, found, err := unstructured.NestedInt64(manager.Object, "spec", "replicas")
	if err != nil || !found {
		t.Fatal("retained manager replica count is unavailable")
	}
	manager.Object["status"] = map[string]any{"observedGeneration": int64(1), "replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas}
	fake.resources[tenantStageKey(manager)] = manager
	return fake
}

func (f *tenantStageFake) VerifyIdentity(ctx context.Context) error {
	f.events = append(f.events, "identity")
	return ctx.Err()
}

func (f *tenantStageFake) matchesBinding(identity generation.ManagementIdentity, version string) bool {
	intent := f.execution.plan.Intent()
	return identity == intent.Management && version == intent.KubernetesVersion
}

func (f *tenantStageFake) Get(_ context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.events = append(f.events, "get:"+wanted.GetKind())
	if current := f.resources[tenantStageKey(wanted)]; current != nil {
		return current.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: strings.ToLower(wanted.GetKind())}, wanted.GetName())
}

func (f *tenantStageFake) Create(_ context.Context, desired *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	f.events = append(f.events, "create:"+desired.GetKind())
	f.createCount++
	created := desired.DeepCopy()
	created.SetUID(types.UID("cluster-created"))
	created.SetResourceVersion("1")
	f.resources[tenantStageKey(created)] = created
	if created.GetKind() == "DPUCluster" {
		f.clusters = append(f.clusters, *created.DeepCopy())
	}
	return created, nil
}

func (f *tenantStageFake) List(_ context.Context, template *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if template.GetKind() != "DPUCluster" || selector != "" {
		return nil, fmt.Errorf("unexpected tenant list request")
	}
	f.listCount++
	f.events = append(f.events, "list")
	items := make([]unstructured.Unstructured, len(f.clusters))
	for i := range f.clusters {
		items[i] = *f.clusters[i].DeepCopy()
	}
	if f.listHook != nil {
		f.listHook(f.listCount)
	}
	return items, nil
}

func (f *tenantStageFake) checkTenantStorage(_ context.Context, namespace string) PreflightCheck {
	f.events = append(f.events, "storage")
	f.storageNamespaces = append(f.storageNamespaces, namespace)
	i := min(f.storageCount, len(f.storage)-1)
	f.storageCount++
	return f.storage[i]
}

func (f *tenantStageFake) connectTenant(ctx context.Context, _ *generation.Plan, uid types.UID, _ *Reconciler, _ *executionJournal, _, _ time.Duration) (*tenantClient, error) {
	f.events = append(f.events, "connect")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.connectUID = uid
	return f.tenant, nil
}

func newTenantStageFixture(t *testing.T) (*managementExecution, *tenantStageFake, managementBootstrapReceipt) {
	t.Helper()
	input := preflightGenerationInputs(t)
	setSiteAllocation(&input.Operator.Bootstrap.Tenant)
	generated, err := generation.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fake := newTenantStageFake(t, execution)
	return execution, fake, tenantStageBootstrapReceipt(execution, fake)
}

func tenantStageBootstrapReceipt(execution *managementExecution, fake *tenantStageFake) managementBootstrapReceipt {
	namespaces := make(map[string]string, len(execution.namespaces))
	for _, wanted := range execution.namespaces {
		namespaces[wanted.GetName()] = string(fake.resources[tenantStageKey(wanted)].GetUID())
	}
	return managementBootstrapReceipt{BundleDigest: execution.plan.Digest(), OperatorNamespace: execution.operatorNamespace, NamespaceUIDs: namespaces,
		OperatorRelease: execution.operator.releaseName, ManagerDeploymentName: execution.manager.GetName(), ManagerDeploymentUID: "manager-uid",
		OperatorConfigName: execution.configObject.GetName(), OperatorConfigUID: "config-uid"}
}

func tenantDesiredObject(t *testing.T, execution *managementExecution) *unstructured.Unstructured {
	t.Helper()
	_, desired, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	return desired
}

func tenantStageKey(object *unstructured.Unstructured) string {
	return object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
}

func testJournal(execution *managementExecution) *executionJournal {
	return &executionJournal{file: &managementMemoryFile{}, digest: execution.plan.Digest()}
}

func tenantStageContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func setSiteAllocation(tenant *bootstrap.Tenant) {
	truth := true
	tenant.Allocation = &bootstrap.SiteAllocation{VIPReserved: &truth, VIPExcludedFromDHCP: &truth, RouterIDUniqueOnSelectedNodes: &truth}
}

func requireOrderedTenantEvents(t *testing.T, events []string, sequence ...string) {
	t.Helper()
	position := 0
	for _, expected := range sequence {
		found := false
		for position < len(events) {
			if events[position] == expected {
				position++
				found = true
				break
			}
			position++
		}
		if !found {
			t.Fatalf("event %q absent/out of order in %v", expected, events)
		}
	}
}
