// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Hooks model producer transitions at semantic boundaries, not API call counts.
type activationLifecycleClient struct {
	*activationIntegrationFake
	afterCreate      func(*activationIntegrationFake)
	afterHold        func(*activationIntegrationFake, string)
	beforeList       func(*activationIntegrationFake, string)
	onGet            func(*activationIntegrationFake, *unstructured.Unstructured)
	listError        string
	emptyFirst       string
	emptySeen        bool
	nilCompleted     bool
	returnedFinalNil bool
}

func (c *activationLifecycleClient) Create(ctx context.Context, desired *unstructured.Unstructured, dry bool) (*unstructured.Unstructured, error) {
	object, err := c.activationIntegrationFake.Create(ctx, desired, dry)
	if err == nil && desired.GetKind() == "DPUDeployment" && c.afterCreate != nil {
		c.afterCreate(c.activationIntegrationFake)
	}
	return object, err
}
func (c *activationLifecycleClient) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if c.onGet != nil {
		c.onGet(c.activationIntegrationFake, desired)
	}
	return c.activationIntegrationFake.Get(ctx, desired)
}
func (c *activationLifecycleClient) List(ctx context.Context, desired *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if c.beforeList != nil {
		c.beforeList(c.activationIntegrationFake, desired.GetKind())
	}
	if c.deployment != nil && c.listError == desired.GetKind() {
		return nil, fmt.Errorf("bounded list denied or incomplete")
	}
	if c.deployment != nil && c.emptyFirst == desired.GetKind() && !c.emptySeen {
		c.emptySeen = true
		return nil, nil
	}

	if c.nilCompleted && c.deployment != nil && desired.GetKind() == "DPUNodeMaintenance" && len(c.maint) == 0 {
		c.returnedFinalNil = true
		return nil, nil
	}
	return c.activationIntegrationFake.List(ctx, desired, selector)
}
func (c *activationLifecycleClient) patchActivationAnnotation(ctx context.Context, object *unstructured.Unstructured, uid types.UID, rv, key string, value *string) (*unstructured.Unstructured, error) {
	updated, err := c.activationIntegrationFake.patchActivationAnnotation(ctx, object, uid, rv, key, value)
	if err == nil && key == activationHoldKey && value != nil && *value == "false" && c.afterHold != nil {
		c.afterHold(c.activationIntegrationFake, object.GetLabels()[activationDPUNodeLabel])
	}
	return updated, err
}

func bindActivationLifecycleClient(f *activationIntegrationFixture, c *activationLifecycleClient) {
	f.controlPlane.tenant.Client.source = c
	f.workload.ControlPlane.tenant.Client.source = c
	f.client.tenant.source = c
	f.client.base.tenant.source = c
}

func activationSuccessfulReboot(client *activationIntegrationFake, calls *int) ExternalRebooter {
	return RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		(*calls)++
		for _, node := range request.DPUNodes {
			client.completeReboot(node.NodeName)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
	})
}

func TestActivationControllerWireLifecycle(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, sameHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-host-%t", sameHost), func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, sameHost)
			calls := 0
			receipt, err := runActivationStage(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
			if err != nil || calls != len(f.hosts) || f.client.guardCalls != len(f.hosts) || len(f.client.maint) != 0 || len(receipt.Maintenance) != 0 || len(receipt.progress) != uniqueNodeCount(f.client.activationSelected) {
				t.Fatalf("real controller coordination/removal: calls=%d claims=%d live-maintenance=%d receipt=%#v err=%v", calls, f.client.guardCalls, len(f.client.maint), receipt.Maintenance, err)
			}
			for _, dpu := range f.client.dpus {
				if !EvaluateReadiness(&dpu).Ready || !conditionReadiness(&dpu, "NodeEffectRemoved", "DPU_STATUS_STALE", true).Ready {
					t.Fatal("DPU did not reach current Ready and NodeEffectRemoved")
				}
			}
		})
	}
}

// Independent pinned-controller fixture. Expected Service values are assembled
// from retained inputs here, rather than by the production expected-spec helper.
func (f *activationIntegrationFake) seedActivationVersions(deployment *unstructured.Unstructured) error {
	objects, err := retainedWorkloadObjects(f.execution)
	if err != nil {
		return err
	}
	var template, configuration *unstructured.Unstructured
	for _, object := range objects {
		switch object.GetKind() {
		case "DPUServiceTemplate":
			template = object
		case "DPUServiceConfiguration":
			configuration = object
		}
	}
	if template == nil || configuration == nil {
		return fmt.Errorf("fixture retained service inputs missing")
	}
	source, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "source")
	if err != nil {
		return err
	}
	source["path"] = ""
	values, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "values")
	if err != nil {
		return err
	}
	overrides, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "helmChart", "values")
	if err != nil {
		return err
	}
	for key, value := range overrides {
		values[key] = value
	}
	daemon, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "serviceDaemonSet")
	if err != nil {
		return err
	}
	parent := deployment.GetNamespace() + "_" + deployment.GetName()
	expressions := []any{map[string]any{"key": "svc.dpu.nvidia.com/dpuservice-doca-hbn-version", "operator": "In", "values": []any{"doca-hbn-random"}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	daemon["labels"] = map[string]any{hbnServiceReferenceLabel: hbnContainerName}
	daemon["nodeSelector"] = map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": expressions}}}
	labels := f.execution.plan.Intent().Bootstrap.Tenant.Labels
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	clusterExpressions := []any{}
	for _, key := range keys {
		clusterExpressions = append(clusterExpressions, map[string]any{"key": key, "operator": "In", "values": []any{labels[key]}})
	}
	cluster := map[string]any{"matchExpressions": clusterExpressions}
	id := deployment.GetName() + "_doca-hbn_aabbccddee"
	service := runtimeObject("DPUService", "doca-hbn-random", deployment.GetNamespace(), map[string]any{"helmChart": map[string]any{"source": source, "values": values}, "serviceID": id, "serviceDaemonSet": daemon, "interfaces": []any{"doca-hbn-port-0-random", "doca-hbn-port-1-random", "doca-hbn-port-2-random"}, "dpuClusterSelector": cluster, "security": map[string]any{"privileged": true}})
	switches, _, err := unstructured.NestedSlice(deployment.Object, "spec", "serviceChains", "switches")
	if err != nil {
		return err
	}
	for _, raw := range switches {
		sw, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("fixture switch malformed")
		}
		ports, ok := sw["ports"].([]any)
		if !ok {
			return fmt.Errorf("fixture ports malformed")
		}
		for _, rawPort := range ports {
			port, ok := rawPort.(map[string]any)
			if !ok {
				return fmt.Errorf("fixture port malformed")
			}
			if rawSvc, exists := port["service"]; exists {
				svc, ok := rawSvc.(map[string]any)
				if !ok {
					return fmt.Errorf("fixture service port malformed")
				}
				name, ok := svc["interface"].(string)
				if !ok {
					return fmt.Errorf("fixture port name missing")
				}
				delete(port, "service")
				port["serviceInterface"] = map[string]any{"matchLabels": map[string]any{hbnServiceIDLabel: id, "svc.dpu.nvidia.com/interface": name}}
			}
		}
	}
	chainExpressions := []any{map[string]any{"key": "svc.dpu.nvidia.com/dpuservicechain-version", "operator": "In", "values": []any{deployment.GetName() + "-current"}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	chain := runtimeObject("DPUServiceChain", deployment.GetName()+"-current", deployment.GetNamespace(), map[string]any{"dpuClusterSelector": cluster, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeSelector": map[string]any{"matchExpressions": chainExpressions}, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"switches": switches}}}}})
	controller := true
	for _, object := range []*unstructured.Unstructured{service, chain} {
		object.SetUID(types.UID("version-" + object.GetName()))
		if object.GetKind() == "DPUService" {
			object.SetUID("service-uid")
		}
		object.SetGeneration(1)
		object.SetResourceVersion("1")
		object.SetLabels(map[string]string{activationParentLabel: parent})
		object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: deployment.GetAPIVersion(), Kind: deployment.GetKind(), Name: deployment.GetName(), UID: deployment.GetUID(), Controller: &controller}})
	}
	service.SetLabels(map[string]string{activationParentLabel: parent, hbnServiceReferenceLabel: hbnContainerName})
	service.SetAnnotations(map[string]string{hbnServiceVersionAnnotation: "aabbccddee"})
	chain.SetAnnotations(map[string]string{"svc.dpu.nvidia.com/dpuservicechain-version": "aabbccddee"})
	f.versions = []unstructured.Unstructured{}
	for _, object := range []*unstructured.Unstructured{service, chain} {
		normalized, e := f.execution.reconciler.schemas.DefaultedCopy(object)
		if e != nil {
			return e
		}
		f.versions = append(f.versions, *normalized)
	}
	return nil
}

func completeActivationWithoutReset(f *activationIntegrationFake, nodeName string) {
	for i := range f.dpus {
		name, _, _ := unstructured.NestedString(f.dpus[i].Object, "spec", "dpuNodeName")
		if name == nodeName {
			setActivationDPUReady(&f.dpus[i])
			unstructured.RemoveNestedField(f.dpus[i].Object, "status", "rebootStatus")
		}
	}
	key := "DPUNode/" + f.deployment.GetNamespace() + "/" + nodeName
	node := f.base.resources[key].DeepCopy()
	annotations := node.GetAnnotations()
	delete(annotations, activationExternalRebootKey)
	node.SetAnnotations(annotations)
	setDPUNodeReady(node, "True")
	f.base.resources[key] = node
	remaining := []unstructured.Unstructured{}
	for _, maintenance := range f.maint {
		if maintenance.GetLabels()[activationDPUNodeLabel] == nodeName {
			f.removedMaintenance = append(f.removedMaintenance, *maintenance.DeepCopy())
		} else {
			remaining = append(remaining, maintenance)
		}
	}
	f.maint = remaining
	ready := true
	for _, dpu := range f.dpus {
		if !EvaluateReadiness(&dpu).Ready {
			ready = false
		}
	}
	if ready {
		setActivationReady(f.deployment)
		f.base.resources[tenantStageKey(f.deployment)] = f.deployment.DeepCopy()
	}
}

func TestActivationRemovalAllowsMissedControllerSnapshotsWithoutReset(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, mode := range []string{"poll-jump", "deleting-then-absent", "nil-empty-lists", "requestors-reordered", "replaced-after-first-release", "version-replaced-after-first-release"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client, nilCompleted: mode == "nil-empty-lists"}
			c.afterCreate = func(base *activationIntegrationFake) {
				for i := range base.maint {
					_ = unstructured.SetNestedSlice(base.maint[i].Object, []any{conditionMap("NodeEffectApplied", "False", "Progressing", 1)}, "status", "conditions")
					if mode == "requestors-reordered" {
						rows, _, _ := unstructured.NestedStringSlice(base.maint[i].Object, "spec", "requestor")
						for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
							rows[left], rows[right] = rows[right], rows[left]
						}
						_ = unstructured.SetNestedStringSlice(base.maint[i].Object, rows, "spec", "requestor")
					}
				}
				for i := range base.dpus {
					_ = unstructured.SetNestedSlice(base.dpus[i].Object, []any{conditionMap("NodeEffectReady", "False", "Progressing", 1)}, "status", "conditions")
				}
			}
			pending := map[string]bool{}
			sawDeleting := false
			c.afterHold = func(base *activationIntegrationFake, node string) {
				if mode == "deleting-then-absent" {
					for i := range base.maint {
						if base.maint[i].GetLabels()[activationDPUNodeLabel] == node {
							timestamp := metav1.Now()
							base.maint[i].SetDeletionTimestamp(&timestamp)
							unstructured.RemoveNestedField(base.maint[i].Object, "spec", "requestor")
							pending[node] = true
						}
					}
					return
				}
				completeActivationWithoutReset(base, node)
				if mode == "nil-empty-lists" && len(base.maint) == 0 {
					base.maint = nil
				}
				if mode == "version-replaced-after-first-release" && len(base.removedMaintenance) == 1 {
					base.versions[0].SetUID("foreign-version-replacement")
				}
				if mode == "replaced-after-first-release" && len(base.removedMaintenance) == 1 {
					foreign := base.removedMaintenance[0].DeepCopy()
					foreign.SetUID("foreign-replacement")
					base.maint = append(base.maint, *foreign)
				}
			}
			c.beforeList = func(base *activationIntegrationFake, kind string) {
				if mode != "deleting-then-absent" || kind != "DPUNodeMaintenance" || len(pending) == 0 {
					return
				}
				if !sawDeleting {
					sawDeleting = true
					return
				}
				for node := range pending {
					completeActivationWithoutReset(base, node)
					delete(pending, node)
				}
				sawDeleting = false
			}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			receipt, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
				calls++
				return ActivationObservation{}, fmt.Errorf("unexpected reset")
			}))
			if mode == "replaced-after-first-release" || mode == "version-replaced-after-first-release" {
				if err == nil || len(c.patches) != 1 || c.guardCalls != 0 || calls != 0 {
					t.Fatalf("foreign recurrence authorized effects: err=%v patches=%v claims=%d calls=%d", err, c.patches, c.guardCalls, calls)
				}
				return
			}
			if mode == "nil-empty-lists" && !c.returnedFinalNil {
				t.Fatal("completion did not receive an actual nil-success List")
			}
			if err != nil || calls != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || len(receipt.DPUs) != 2 || len(receipt.Maintenance) != 0 || len(c.patches) != uniqueNodeCount(c.activationSelected) {
				t.Fatalf("missed intermediate should finish only from actual releases+current removed/ready: err=%v calls=%d claims=%d patches=%v", err, calls, c.guardCalls, c.patches)
			}
		})
	}
}

func TestActivationUnknownCompletionHistoryCannotAuthorizeEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mode := range []string{"initial-false-no-reset", "initial-absent", "missing-removed-condition", "stale-removed-condition", "missing-ready-condition"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client}
			c.afterCreate = func(base *activationIntegrationFake) {
				if mode == "initial-false-no-reset" || mode == "initial-absent" {
					for i := range base.maint {
						a := base.maint[i].GetAnnotations()
						a[activationHoldKey] = "false"
						base.maint[i].SetAnnotations(a)
					}
					completeActivationWithoutReset(base, base.activationSelected[0].nodeName)
					if mode == "initial-false-no-reset" {
						base.maint = deepCopyUnstructuredList(base.removedMaintenance)
					}
					return
				}
			}
			if strings.HasPrefix(mode, "missing-") || mode == "stale-removed-condition" {
				c.afterHold = func(base *activationIntegrationFake, node string) {
					completeActivationWithoutReset(base, node)
					conditions := []any{conditionMap("NodeEffectReady", "True", "Success", 1), conditionMap("NodeEffectRemoved", "True", "Success", 1), conditionMap("Ready", "True", "Success", 1)}
					if mode == "missing-removed-condition" {
						conditions = append(conditions[:1], conditions[2:]...)
					}
					if mode == "stale-removed-condition" {
						conditions[1] = conditionMap("NodeEffectRemoved", "True", "Success", 0)
					}
					if mode == "missing-ready-condition" {
						conditions = conditions[:2]
					}
					_ = unstructured.SetNestedSlice(base.dpus[0].Object, conditions, "status", "conditions")
				}
			}
			bindActivationLifecycleClient(&f, c)
			ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
			defer cancel()
			calls := 0
			_, err := runActivationStage(ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
				calls++
				return ActivationObservation{}, nil
			}))
			if err == nil || c.guardCalls != 0 || calls != 0 {
				t.Fatalf("unknown/incomplete history authorized effects: err=%v claims=%d calls=%d", err, c.guardCalls, calls)
			}
			if !strings.HasPrefix(mode, "initial-") && len(c.patches) != 1 {
				t.Fatalf("negative did not reach the intended post-release boundary: patches=%v err=%v", c.patches, err)
			}
			if strings.HasPrefix(mode, "initial-") && (len(c.patches) != 0 || !strings.Contains(err.Error(), "ACTIVATION_COMPLETION_HISTORY_UNKNOWN")) {
				t.Fatalf("bare current state minted history: err=%v patches=%v", err, c.patches)
			}
		})
	}
}

type activationGraphObjects struct {
	versions, sets, dpus, maint []unstructured.Unstructured
	deployment                  *unstructured.Unstructured
}

func TestActivationVersionGraphRejectsForeignOrChangedCoordination(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	cases := []struct {
		name   string
		mutate func(*activationGraphObjects)
	}{
		{"service-source", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedField(f.versions[0].Object, "https://foreign.invalid/chart", "spec", "helmChart", "source", "repoURL")
		}},
		{"service-config", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedField(f.versions[0].Object, "foreign", "spec", "helmChart", "values", "image", "repository")
		}},
		{"service-logical-reference", func(f *activationGraphObjects) {
			labels := f.versions[0].GetLabels()
			labels[hbnServiceReferenceLabel] = "foreign"
			f.versions[0].SetLabels(labels)
		}},
		{"service-owner", func(f *activationGraphObjects) {
			owners := f.versions[0].GetOwnerReferences()
			owners[0].UID = "foreign"
			f.versions[0].SetOwnerReferences(owners)
		}},
		{"service-id", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedField(f.versions[0].Object, "foreign", "spec", "serviceID")
		}},
		{"service-selector", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedMap(f.versions[0].Object, map[string]any{}, "spec", "serviceDaemonSet", "nodeSelector")
		}},
		{"service-cluster", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedMap(f.versions[0].Object, map[string]any{}, "spec", "dpuClusterSelector")
		}},
		{"service-duplicate-interfaces", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedStringSlice(f.versions[0].Object, []string{"same", "same", "other"}, "spec", "interfaces")
		}},
		{"chain-switch", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedSlice(f.versions[1].Object, []any{}, "spec", "template", "spec", "template", "spec", "switches")
		}},
		{"chain-owner", func(f *activationGraphObjects) {
			owners := f.versions[1].GetOwnerReferences()
			owners[0].UID = "foreign"
			f.versions[1].SetOwnerReferences(owners)
		}},
		{"chain-selector", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedMap(f.versions[1].Object, map[string]any{}, "spec", "template", "spec", "nodeSelector")
		}},
		{"chain-version", func(f *activationGraphObjects) { f.versions[1].SetAnnotations(nil) }},
		{"set-extra-participant", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedStringSlice(f.sets[0].Object, []string{"foreign"}, "spec", "dpuTemplate", "spec", "nodeEffect", "nodeMaintenanceAdditionalRequestors")
		}},
		{"set-version", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedStringMap(f.sets[0].Object, map[string]string{activationParentLabel: "foreign"}, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
		}},
		{"dpu-participant", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedStringSlice(f.dpus[0].Object, []string{"foreign"}, "spec", "nodeEffect", "nodeMaintenanceAdditionalRequestors")
		}},
		{"dpu-force", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedField(f.dpus[0].Object, true, "spec", "nodeEffect", "force")
		}},
		{"dpu-cluster-labels", func(f *activationGraphObjects) {
			_ = unstructured.SetNestedStringMap(f.dpus[0].Object, map[string]string{activationParentLabel: "foreign"}, "spec", "cluster", "nodeLabels")
		}},
		{"foreign-requestor-prefix", func(f *activationGraphObjects) {
			rows, _, _ := unstructured.NestedStringSlice(f.maint[0].Object, "spec", "requestor")
			rows = append(rows, f.deployment.GetNamespace()+"_"+f.deployment.GetName()+"_unqualified")
			_ = unstructured.SetNestedStringSlice(f.maint[0].Object, rows, "spec", "requestor")
		}},
		{"duplicate-requestor", func(f *activationGraphObjects) {
			rows, _, _ := unstructured.NestedStringSlice(f.maint[0].Object, "spec", "requestor")
			rows = append(rows, rows[0])
			_ = unstructured.SetNestedStringSlice(f.maint[0].Object, rows, "spec", "requestor")
		}},
	}
	baseline := newActivationIntegrationFixtureFromExecution(t, execution, false)
	desired, err := retainedDeploymentObject(execution)
	if err != nil {
		t.Fatal(err)
	}
	created, err := baseline.client.Create(baseline.ctx, desired, false)
	if err != nil {
		t.Fatal(err)
	}
	for index, test := range cases {
		if index < 12 {
			t.Run(test.name+"-projection", func(t *testing.T) {
				objects := activationGraphObjects{versions: deepCopyUnstructuredList(baseline.client.versions), sets: deepCopyUnstructuredList(baseline.client.sets), deployment: created.DeepCopy()}
				test.mutate(&objects)
				sets := map[string]*unstructured.Unstructured{}
				for i := range objects.sets {
					sets[objects.sets[i].GetName()] = &objects.sets[i]
				}
				before := objects.versions[0].DeepCopy()
				beforeChain := objects.versions[1].DeepCopy()
				_, complete, err := compileActivationVersionGraph(execution, objects.deployment, sets, objects.versions[:1], objects.versions[1:])
				if err == nil || complete {
					t.Fatalf("foreign graph projection accepted: complete=%v err=%v", complete, err)
				}
				if !reflect.DeepEqual(before, objects.versions[0].DeepCopy()) || !reflect.DeepEqual(beforeChain, objects.versions[1].DeepCopy()) {
					t.Fatal("projection mutated inputs")
				}
			})
		}
		if index == 0 || index == 8 || index >= 12 {
			t.Run(test.name+"-stage", func(t *testing.T) {
				f := newActivationIntegrationFixtureFromExecution(t, execution, false)
				c := &activationLifecycleClient{activationIntegrationFake: f.client, afterCreate: func(base *activationIntegrationFake) {
					test.mutate(&activationGraphObjects{versions: base.versions, sets: base.sets, dpus: base.dpus, maint: base.maint, deployment: base.deployment})
				}}
				bindActivationLifecycleClient(&f, c)
				calls := 0
				_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
				if c.deployment == nil || err == nil || len(c.patches) != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || calls != 0 {
					t.Fatalf("coordination drift reached effects: err=%v patches=%v claims=%d callbacks=%d", err, c.patches, c.guardCalls, calls)
				}
			})
		}
	}
}

func TestActivationCompleteListResultsAreRequiredBeforeEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, kind := range []string{"DPUSet", "DPU", "DPUNodeMaintenance", "DPUService", "DPUServiceChain"} {
		t.Run(kind, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client, emptyFirst: kind}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
			if !c.emptySeen || err != nil || calls != 1 {
				t.Fatalf("successful empty list must remain pending, not terminal or ready: err=%v empty=%v calls=%d", err, c.emptySeen, calls)
			}
		})
	}
	for _, kind := range []string{"DPUService", "DPUServiceChain"} {
		t.Run(kind+"-denied", func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client, listError: kind}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
			if c.deployment == nil || err == nil || calls != 0 || c.guardCalls != 0 || len(c.patches) != 0 {
				t.Fatalf("failed list authorized effects: err=%v calls=%d claims=%d patches=%v", err, calls, c.guardCalls, c.patches)
			}
		})
	}
}

func TestActivationVersionPermissionsCoverBothEarlyBoundaries(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server.URL)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server.URL)
	result := client.CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if !result.Ready {
		t.Fatal("complete permission fixture unexpectedly denied")
	}
	for _, resource := range []string{"dpuservices", "dpuservicechains"} {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			a := review.Spec.ResourceAttributes
			return a != nil && a.Verb == "list" && a.Group == "svc.dpu.nvidia.com" && a.Version == "v1alpha1" && a.Resource == resource && a.Namespace == "dpf-operator-system"
		})
	}
	harness.reviews = nil
	checks := client.checkActivationPermissions(context.Background(), &managementExecution{operatorNamespace: "dpf-operator-system"})
	if len(checks) == 0 {
		t.Fatal("missing activation permission checks")
	}
	for _, resource := range []string{"dpuservices", "dpuservicechains"} {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			a := review.Spec.ResourceAttributes
			return a != nil && a.Verb == "list" && a.Group == "svc.dpu.nvidia.com" && a.Version == "v1alpha1" && a.Resource == resource && a.Namespace == "dpf-operator-system"
		})
	}
}

func TestHBNServiceProjectionKeepsRawAppInputsIndependent(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	deployment, err := retainedDeploymentObject(f.prepared.execution)
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.client.Create(f.ctx, deployment, false)
	if err != nil {
		t.Fatal(err)
	}
	var template, configuration *unstructured.Unstructured
	objects, err := retainedWorkloadObjects(f.prepared.execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		switch object.GetKind() {
		case "DPUServiceTemplate":
			template = object
		case "DPUServiceConfiguration":
			configuration = object
		}
	}
	if template == nil || configuration == nil {
		t.Fatal("retained inputs missing")
	}
	templateBefore := template.DeepCopy()
	service := &f.client.versions[0]
	selector, _, err := unstructured.NestedMap(service.Object, "spec", "serviceDaemonSet", "nodeSelector")
	if err != nil {
		t.Fatal(err)
	}
	cluster, _, err := unstructured.NestedMap(service.Object, "spec", "dpuClusterSelector")
	if err != nil {
		t.Fatal(err)
	}
	names, _, err := unstructured.NestedSlice(service.Object, "spec", "interfaces")
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := unstructured.NestedString(service.Object, "spec", "serviceID")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise absent raw daemonSet annotations; this is the producer's App input,
	// even when a separate Service-normalization result is subsequently changed.
	unstructured.RemoveNestedField(configuration.Object, "spec", "serviceConfiguration", "serviceDaemonSet", "annotations")
	configurationBefore := configuration.DeepCopy()
	projection, err := compileHBNServiceSpec(f.prepared.execution, template, configuration, service.GetName(), created.GetNamespace(), hbnContainerName, id, selector, cluster, names)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := projection.daemonSet["annotations"]; exists {
		t.Fatal("raw App inputs acquired an absent Service field")
	}
	rawBefore, err := json.Marshal([]any{projection.source, projection.values, projection.daemonSet})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(projection.spec, "schema-only", "serviceDaemonSet", "labels", "test")
	_ = unstructured.SetNestedField(projection.spec, "schema-only", "helmChart", "values", "test")
	_ = unstructured.SetNestedField(projection.spec, "schema-only", "helmChart", "source", "repoURL")
	rawAfter, err := json.Marshal([]any{projection.source, projection.values, projection.daemonSet})
	if err != nil {
		t.Fatal(err)
	}
	if string(rawBefore) != string(rawAfter) || !reflect.DeepEqual(template, templateBefore) || !reflect.DeepEqual(configuration, configurationBefore) {
		t.Fatal("Service normalization aliases raw App or retained inputs")
	}
	// Raw outputs also own their maps independently of retained source inputs.
	projection.values["raw-only"] = "independent"
	projection.daemonSet["raw-only"] = "independent"
	projection.source["repoURL"] = "raw-only"
	if !reflect.DeepEqual(template, templateBefore) || !reflect.DeepEqual(configuration, configurationBefore) {
		t.Fatal("raw output mutation escaped into retained input")
	}
}

func TestActivationFinalReceiptReobservesAfterDeploymentWait(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mode := range []string{"replaced-dpu", "current-generation", "new-node-request"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client}
			waited, changed := false, false
			c.onGet = func(base *activationIntegrationFake, want *unstructured.Unstructured) {
				if changed || want.GetKind() != "DPUDeployment" || base.deployment == nil || len(base.removedMaintenance) == 0 || len(base.maint) != 0 {
					return
				}
				recorded := false
				for _, line := range strings.Split(strings.TrimSpace(f.journal.file.(*managementMemoryFile).String()), "\n") {
					var event executionEvent
					if json.Unmarshal([]byte(line), &event) == nil && event.Stage == generation.StageDeployment && event.Operation == "readiness" && event.Subject == base.deployment.GetNamespace()+"/"+base.deployment.GetName() && event.State == "waiting" {
						recorded = true
					}
				}
				if !recorded {
					_ = unstructured.SetNestedSlice(base.deployment.Object, []any{conditionMap("Ready", "False", "Progressing", 1)}, "status", "conditions")
					base.base.resources[tenantStageKey(base.deployment)] = base.deployment.DeepCopy()
					return
				}
				waited, changed = true, true
				switch mode {
				case "replaced-dpu":
					base.dpus[0].SetUID("replacement-during-deployment-wait")
				case "current-generation":
					base.dpus[0].SetGeneration(2)
					_ = unstructured.SetNestedField(base.dpus[0].Object, int64(2), "status", "observedGeneration")
					_ = unstructured.SetNestedSlice(base.dpus[0].Object, []any{conditionMap("Ready", "True", "Success", 2), conditionMap("NodeEffectReady", "True", "Success", 2), conditionMap("NodeEffectRemoved", "True", "Success", 2)}, "status", "conditions")
				case "new-node-request":
					node := base.base.resources["DPUNode/"+base.deployment.GetNamespace()+"/"+base.activationSelected[0].nodeName]
					annotations := node.GetAnnotations()
					annotations[activationExternalRebootKey] = "true"
					node.SetAnnotations(annotations)
				}
				setActivationReady(base.deployment)
				base.base.resources[tenantStageKey(base.deployment)] = base.deployment.DeepCopy()
			}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			receipt, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
			if !waited || !changed || calls != 1 || c.guardCalls != 1 || len(c.patches) != 2 {
				t.Fatalf("test did not cross exactly one release/reset/ACK and DD wait: waited=%v changed=%v calls=%d claims=%d patches=%v err=%v", waited, changed, calls, c.guardCalls, c.patches, err)
			}
			report := f.journal.file.(*managementMemoryFile).String()
			if mode == "current-generation" {
				if err != nil || len(receipt.DPUs) != 1 || receipt.DPUs[0].Generation != 2 {
					t.Fatalf("receipt did not bind fresh current generation: %#v err=%v", receipt.DPUs, err)
				}
			} else {
				if err == nil || strings.Contains(report, "SELECTED_DPU_ACTIVATION_COMPLETE") {
					t.Fatalf("changed final graph produced completion: err=%v", err)
				}
			}
		})
	}
}

func TestActivationCoordinationDiagnosticsAreFiniteAndPrivate(t *testing.T) {
	const canary = "private-coordination-diagnostic-canary"
	if strings.Contains(activationCoordinationError(canary).Error(), canary) {
		t.Fatal("unknown constructor category escaped")
	}

	var typedNil *ReconciliationError
	causes := []struct {
		name   string
		cause  error
		reason string
	}{
		{"nil", nil, ""}, {"typed-nil", typedNil, ""}, {"ordinary", errors.New(canary), ""}, {"unknown", &ReconciliationError{ReasonCode: canary, Message: canary, cause: errors.New(canary)}, ""},
		{"cancel", fmt.Errorf("%s: %w", canary, context.Canceled), ""}, {"deadline", fmt.Errorf("%s: %w", canary, context.DeadlineExceeded), ""},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "svc.dpu.nvidia.com", Resource: "dpuservices"}, canary, errors.New(canary)), ""},
	}
	for _, code := range []string{activationParticipantUnsupportedPolicy, activationParticipantGraph, activationParticipantList, activationParticipantOwner, activationParticipantSelector, activationParticipantServiceSpec, activationParticipantChainSpec, activationParticipantPolicy, activationParticipantRequestors, activationParticipantMaintenance, "ACTIVATION_REBOOT_SCOPE_CHANGED"} {
		causes = append(causes, struct {
			name   string
			cause  error
			reason string
		}{name: code, cause: fmt.Errorf("%s: %w", canary, &ReconciliationError{ReasonCode: code, Message: canary, cause: errors.New(canary)}), reason: code})
	}
	for _, test := range causes {
		t.Run(test.name, func(t *testing.T) {
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			result := activationFailure(journal, "ACTIVATION_SCOPE_CHANGED", "The selected scope could not be verified.", test.cause)
			if result == nil || strings.Contains(result.Error(), canary) || strings.Contains(file.String(), canary) {
				t.Fatalf("private cause text escaped: %v", result)
			}
			var outer *ReconciliationError
			if !errors.As(result, &outer) || outer == nil || outer.ReasonCode != "ACTIVATION_SCOPE_CHANGED" {
				t.Fatal("outer failure category changed")
			}
			if test.cause != nil && !errors.Is(result, test.cause) {
				t.Fatal("original error classification lost")
			}
			if test.name == "cancel" && !errors.Is(result, context.Canceled) || test.name == "deadline" && !errors.Is(result, context.DeadlineExceeded) || test.name == "forbidden" && !apierrors.IsForbidden(result) {
				t.Fatal("cancellation/deadline/API classification changed")
			}
			var event executionEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(file.String())), &event); err != nil {
				t.Fatal(err)
			}
			if event.State != "failed" || event.ReasonCode != "ACTIVATION_SCOPE_CHANGED" {
				t.Fatal("outer journal state/category changed")
			}
			if test.reason != "" && (!strings.Contains(event.Message, activationCoordinationText(test.reason)) || !strings.Contains(event.Message, test.reason)) {
				t.Fatal("finite human reason missing")
			}
		})
	}
}

func TestActivationFirstFalseRequiresActualResetProofAndOwnParticipant(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mode := range []string{"qualified-reset-after-controller-shrink", "uncertain-reset-no-history", "own-requestor-missing-before-reset"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client}
			c.afterCreate = func(base *activationIntegrationFake) {
				if mode == "own-requestor-missing-before-reset" {
					return
				}
				for i := range base.maint {
					a := base.maint[i].GetAnnotations()
					a[activationHoldKey] = "false"
					base.maint[i].SetAnnotations(a)
					_ = unstructured.SetNestedStringSlice(base.maint[i].Object, []string{base.activationSelected[0].dpuName}, "spec", "requestor")
				}
			}
			if mode == "own-requestor-missing-before-reset" {
				c.afterHold = func(base *activationIntegrationFake, _ string) {
					rows, _, _ := unstructured.NestedStringSlice(base.maint[0].Object, "spec", "requestor")
					remaining := []string{}
					for _, row := range rows {
						if row != base.activationSelected[0].dpuName {
							remaining = append(remaining, row)
						}
					}
					_ = unstructured.SetNestedStringSlice(base.maint[0].Object, remaining, "spec", "requestor")
				}
			}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			reboot := activationSuccessfulReboot(f.client, &calls)
			if mode == "uncertain-reset-no-history" {
				reboot = RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
					calls++
					return ActivationObservation{}, fmt.Errorf("lost response")
				})
			}
			receipt, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
			switch mode {
			case "qualified-reset-after-controller-shrink":
				if err != nil || calls != 1 || c.guardCalls != 1 || len(c.patches) != 1 || c.patches[0] != activationExternalRebootKey || len(receipt.progress) != 1 {
					t.Fatalf("qualified current reset could not establish completion: err=%v calls=%d claims=%d patches=%v", err, calls, c.guardCalls, c.patches)
				}
				for _, proof := range receipt.progress {
					if proof.releasedVersion != "" || !proof.resetAcknowledged {
						t.Fatal("false hold was mislabeled as own release")
					}
				}
			case "uncertain-reset-no-history":
				if err == nil || calls != 1 || c.guardCalls != 1 || len(receipt.progress) != 0 || len(c.patches) != 0 {
					t.Fatalf("uncertain reset minted history: err=%v receipt=%#v", err, receipt.progress)
				}
				_, retryErr := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, testJournal(execution), time.Millisecond, time.Second, reboot)
				if retryErr == nil || calls != 1 {
					t.Fatal("uncertain same-round reset replayed")
				}
			case "own-requestor-missing-before-reset":
				if err == nil || !strings.Contains(err.Error(), "ACTIVATION_REBOOT_SCOPE_CHANGED") || calls != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || len(c.patches) != 1 {
					t.Fatalf("missing own participant authorized reset: err=%v calls=%d claims=%d patches=%v", err, calls, c.guardCalls, c.patches)
				}
			}
		})
	}
}

func TestActivationRejectsUnsupportedLabelChangePolicyBeforeEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	baseline := newActivationIntegrationFixtureFromExecution(t, execution, false)
	desired, err := retainedDeploymentObject(execution)
	if err != nil {
		t.Fatal(err)
	}
	created, err := baseline.client.Create(baseline.ctx, desired, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name            string
		value           any
		absent, allowed bool
	}{
		{name: "omitted-false-default", absent: true, allowed: true}, {name: "explicit-false", value: false, allowed: true}, {name: "true", value: true}, {name: "string-false", value: "false"}, {name: "null", value: nil}, {name: "numeric-zero", value: int64(0)},
	} {
		t.Run(test.name+"-projection", func(t *testing.T) {
			set := baseline.client.sets[0].DeepCopy()
			if test.absent {
				unstructured.RemoveNestedField(set.Object, "spec", "dpuTemplate", "spec", "nodeEffect", "applyOnLabelChange")
			} else if err := unstructured.SetNestedField(set.Object, test.value, "spec", "dpuTemplate", "spec", "nodeEffect", "applyOnLabelChange"); err != nil {
				t.Fatal(err)
			}
			before := set.DeepCopy()
			_, complete, err := compileActivationVersionGraph(execution, created, map[string]*unstructured.Unstructured{set.GetName(): set}, baseline.client.versions[:1], baseline.client.versions[1:])
			if test.allowed {
				if err != nil || !complete {
					t.Fatalf("pinned false/default rejected: complete=%v err=%v", complete, err)
				}
			} else {
				if err == nil || complete || !strings.Contains(err.Error(), activationParticipantUnsupportedPolicy) {
					t.Fatalf("unsupported upgrade policy accepted: complete=%v err=%v", complete, err)
				}
			}
			if !reflect.DeepEqual(before, set) {
				t.Fatal("policy qualification mutated current Set")
			}
		})
	}
	for _, test := range []struct {
		name     string
		set, dpu any
	}{{"both-true", true, true}, {"dpu-true-mismatch", false, true}, {"malformed-dpu", false, "false"}} {
		t.Run(test.name+"-stage", func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationLifecycleClient{activationIntegrationFake: f.client, afterCreate: func(base *activationIntegrationFake) {
				_ = unstructured.SetNestedField(base.sets[0].Object, test.set, "spec", "dpuTemplate", "spec", "nodeEffect", "applyOnLabelChange")
				_ = unstructured.SetNestedField(base.dpus[0].Object, test.dpu, "spec", "nodeEffect", "applyOnLabelChange")
			}}
			bindActivationLifecycleClient(&f, c)
			calls := 0
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, activationSuccessfulReboot(f.client, &calls))
			if err == nil || len(c.patches) != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || calls != 0 {
				t.Fatalf("unsupported/malformed policy authorized effects: err=%v patches=%v claims=%d callbacks=%d", err, c.patches, c.guardCalls, calls)
			}
		})
	}
}
