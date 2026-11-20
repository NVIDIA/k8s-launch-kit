// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type managementMemoryFile struct{ bytes.Buffer }

func (managementMemoryFile) Sync() error  { return nil }
func (managementMemoryFile) Close() error { return nil }

type managementFake struct {
	bundleDigest             string
	manager                  *unstructured.Unstructured
	crds                     map[string]*unstructured.Unstructured
	resources                map[string][]metav1.APIResource
	namespaces               map[string]*unstructured.Unstructured
	config                   *unstructured.Unstructured
	events                   []string
	preflight                bool
	binding                  bool
	operatorErr              error
	configErr                error
	configVersion            string
	targetVersion            string
	replaceNamespaceOnConfig bool
	alterManagerOnConfig     bool
	failIdentityAfterConfig  bool
	createConfig             int
	configOnRead             func(*unstructured.Unstructured)
	listConfig               int
	listHook                 func(*managementFake)
}

func newManagementFake(t *testing.T, generated *bundle.Bundle) *managementFake {
	t.Helper()
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	inputs, archive := plan.Operator()
	op, err := prepareOperator(OperatorRequest{Inputs: inputs, Archive: archive, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	manager, expectations, err := operatorReadinessObjects(op)
	if err != nil {
		t.Fatal(err)
	}
	return managementFakeWithReadiness(t, plan.Digest(), manager, expectations)
}

// managementFakeWithReadiness copies a validated projection into independent
// simulated live state. Stage fixtures can reuse their execution's projection
// without rendering and parsing the operator chart a second time.
func managementFakeWithReadiness(t *testing.T, digest string, desiredManager *unstructured.Unstructured, expectations []operatorCRDExpectation) *managementFake {
	t.Helper()
	manager := desiredManager.DeepCopy()
	manager.SetUID(types.UID("manager-uid"))
	manager.SetResourceVersion("1")
	manager.SetGeneration(1)
	if !managerDeploymentIntentMatches(manager, manager) {
		t.Fatalf("generated manager did not match itself: %#v", manager.Object)
	}
	replicas, found, replicaErr := unstructured.NestedInt64(manager.Object, "spec", "replicas")
	if replicaErr != nil || !found {
		t.Fatal("generated manager has invalid replica count")
	}
	manager.Object["status"] = map[string]any{"observedGeneration": int64(1), "replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas}
	crds := make(map[string]*unstructured.Unstructured, len(expectations))
	resources := make(map[string][]metav1.APIResource)
	for _, expected := range expectations {
		crd := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": expected.name, "uid": "crd-" + expected.name, "resourceVersion": "1"},
			"spec":     runtime.DeepCopyJSONValue(expected.spec),
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Established", "status": "True"}, map[string]any{"type": "NamesAccepted", "status": "True"},
			}},
		}}
		crds[expected.name] = crd
		for _, version := range expected.versions {
			gv := expected.group + "/" + version
			resources[gv] = append(resources[gv], metav1.APIResource{Name: expected.plural, Kind: expected.kind, Namespaced: expected.namespaced})
		}
	}
	return &managementFake{bundleDigest: digest, manager: manager, crds: crds,
		resources: resources, namespaces: map[string]*unstructured.Unstructured{}, preflight: true, binding: true}
}

func TestManagementFakesOwnIndependentReadinessState(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	desiredManager := execution.manager.DeepCopy()
	const crdName = "dpfoperatorconfigs.operator.dpu.nvidia.com"
	var desiredSpec map[string]any
	for _, expected := range execution.crds {
		if expected.name == crdName {
			desiredSpec = runtime.DeepCopyJSONValue(expected.spec).(map[string]any)
		}
	}
	if desiredSpec == nil {
		t.Fatal("fixture lacks operator configuration CRD")
	}

	first := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
	second := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
	originalManager := second.manager.DeepCopy()
	firstCRD, firstFound := first.crds[crdName]
	secondCRD, secondFound := second.crds[crdName]
	if !firstFound || !secondFound || firstCRD == nil || secondCRD == nil {
		t.Fatal("fake lacks operator configuration CRD")
	}
	originalCRD := secondCRD.DeepCopy()
	// Mutate nested maps in place; copying/replacing a slice here could conceal
	// an alias between a fake's children and the retained projection.
	containerValue, found, err := unstructured.NestedFieldNoCopy(first.manager.Object, "spec", "template", "spec", "containers")
	containers, ok := containerValue.([]any)
	if err != nil || !found || !ok || len(containers) == 0 {
		t.Fatal("fake manager lacks a valid container list")
	}
	container, ok := containers[0].(map[string]any)
	if !ok || container == nil {
		t.Fatal("fake manager container is malformed")
	}
	container["args"] = []any{"--changed"}
	versionValue, found, err := unstructured.NestedFieldNoCopy(firstCRD.Object, "spec", "versions")
	versions, ok := versionValue.([]any)
	if err != nil || !found || !ok || len(versions) == 0 {
		t.Fatal("fake CRD lacks a valid version list")
	}
	version, ok := versions[0].(map[string]any)
	if !ok || version == nil {
		t.Fatal("fake CRD version is malformed")
	}
	version["served"] = false
	for groupVersion, resources := range first.resources {
		if len(resources) == 0 {
			t.Fatalf("fake discovery has no resources for %s", groupVersion)
		}
		resources[0].Name = "changed"
	}
	first.namespaces["changed"] = &unstructured.Unstructured{}
	first.events = append(first.events, "changed")

	if !reflect.DeepEqual(second.manager, originalManager) || !reflect.DeepEqual(second.crds[crdName], originalCRD) || len(second.namespaces) != 0 || len(second.events) != 0 {
		t.Fatal("mutating one fake changed another fake's live state")
	}
	if !reflect.DeepEqual(execution.manager, desiredManager) {
		t.Fatal("mutating a fake changed the retained manager projection")
	}
	for _, expected := range execution.crds {
		if expected.name == crdName && !reflect.DeepEqual(expected.spec, desiredSpec) {
			t.Fatal("mutating a fake changed the retained CRD projection")
		}
	}
	third := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
	if third.bundleDigest != execution.plan.Digest() || !reflect.DeepEqual(third.manager, second.manager) || !reflect.DeepEqual(third.crds, second.crds) || !reflect.DeepEqual(third.resources, second.resources) {
		t.Fatal("a later fake inherited an earlier fake's mutations or lost its plan binding")
	}
}

func (f *managementFake) Get(_ context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.events = append(f.events, "get:"+wanted.GetKind())
	switch wanted.GetKind() {
	case "Namespace":
		if object := f.namespaces[wanted.GetName()]; object != nil {
			return object.DeepCopy(), nil
		}
	case "CustomResourceDefinition":
		if object := f.crds[wanted.GetName()]; object != nil {
			return object.DeepCopy(), nil
		}
	case "Deployment":
		return f.manager.DeepCopy(), nil
	case "DPFOperatorConfig":
		if f.config != nil {
			if f.configOnRead != nil {
				f.configOnRead(f.config)
			}
			return f.config.DeepCopy(), nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: strings.ToLower(wanted.GetKind())}, wanted.GetName())
}

func (f *managementFake) Create(_ context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if dryRun {
		return nil, fmt.Errorf("unexpected dry-run create")
	}
	f.events = append(f.events, "create:"+desired.GetKind())
	switch desired.GetKind() {
	case "Namespace":
		created := desired.DeepCopy()
		created.SetUID(types.UID("namespace-" + desired.GetName()))
		created.SetResourceVersion("1")
		created.Object["status"] = map[string]any{"phase": "Active"}
		f.namespaces[desired.GetName()] = created
		return created.DeepCopy(), nil
	case "DPFOperatorConfig":
		f.createConfig++
		if f.configErr != nil {
			return nil, f.configErr
		}
		created := desired.DeepCopy()
		created.SetUID(types.UID("config-uid"))
		created.SetResourceVersion("1")
		created.SetGeneration(1)
		created.Object["status"] = map[string]any{
			"observedGeneration": int64(1), "version": defaultString(f.configVersion, "v26.8.0"), "targetVersion": defaultString(f.targetVersion, "v26.8.0"),
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}},
		}
		f.config = created
		if f.replaceNamespaceOnConfig {
			for name, namespace := range f.namespaces {
				namespace.SetUID(types.UID("replacement-" + name))
			}
		}
		if f.alterManagerOnConfig {
			containers, _, _ := unstructured.NestedSlice(f.manager.Object, "spec", "template", "spec", "containers")
			containers[0].(map[string]any)["args"] = []any{"--config-name=other"}
			_ = unstructured.SetNestedSlice(f.manager.Object, containers, "spec", "template", "spec", "containers")
		}
		return created.DeepCopy(), nil
	default:
		return nil, fmt.Errorf("unexpected create %s", desired.GetKind())
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (f *managementFake) List(_ context.Context, template *unstructured.Unstructured, _ string) ([]unstructured.Unstructured, error) {
	f.events = append(f.events, "list:"+template.GetKind())
	if template.GetKind() != "DPFOperatorConfig" {
		return nil, fmt.Errorf("unexpected list %s", template.GetKind())
	}
	f.listConfig++
	if f.listHook != nil {
		f.listHook(f)
	}
	if f.config == nil {
		return []unstructured.Unstructured{}, nil
	}
	return []unstructured.Unstructured{*f.config.DeepCopy()}, nil
}

func (f *managementFake) VerifyIdentity(context.Context) error {
	f.events = append(f.events, "verify")
	if f.failIdentityAfterConfig && f.config != nil {
		return errors.New("identity check failed")
	}
	return nil
}

func (f *managementFake) matchesBinding(generation.ManagementIdentity, string) bool { return f.binding }

func (f *managementFake) checkManagementPreflight(context.Context, preflightPlan) PreflightResult {
	f.events = append(f.events, "preflight")
	state := "passed"
	if !f.preflight {
		state = "failed"
	}
	return PreflightResult{Ready: f.preflight, Checks: []PreflightCheck{{ID: "fixture", State: state, Message: "fixture"}}}
}

func (f *managementFake) checkDPFOperatorConfigPermissions(context.Context, string) []PreflightCheck {
	return []PreflightCheck{{ID: "caller.rbac.dpfoperatorconfig.list", State: "passed", Message: "fixture"}}
}

func (f *managementFake) discoverResourceList(_ context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	f.events = append(f.events, "discover:"+groupVersion)
	if groupVersion == "operator.dpu.nvidia.com/v1alpha1" {
		if resources := f.resources[groupVersion]; len(resources) > 0 {
			return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: resources}, nil
		}
		return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: []metav1.APIResource{{Name: "dpfoperatorconfigs", Kind: "DPFOperatorConfig", Namespaced: true}}}, nil
	}
	if resources := f.resources[groupVersion]; len(resources) > 0 {
		return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: resources}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "apiresources"}, groupVersion)
}

func (f *managementFake) ensureOperatorPlan(context.Context, *operatorPlan) error {
	f.events = append(f.events, "helm")
	return f.operatorErr
}

func TestRunManagementValidatesBundleAndBindingBeforeManagementCalls(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newManagementFake(t, generated)
	client.binding = false
	journal := &executionJournal{file: &managementMemoryFile{}, digest: generated.Index().BundleDigest}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = runManagement(ctx, client, generated, journal, time.Millisecond)
	if err == nil || len(client.events) != 0 {
		t.Fatalf("binding mismatch error=%v, API calls=%v", err, client.events)
	}
	client.binding = true
	client.events = nil
	journal = &executionJournal{file: &managementMemoryFile{}, digest: generated.Index().BundleDigest}
	_, err = runManagement(ctx, client, nil, journal, time.Millisecond)
	if err == nil || len(client.events) != 0 {
		t.Fatalf("invalid retained bundle error=%v, API calls=%v", err, client.events)
	}
}

func TestRunManagementPreflightAndHelmFailuresStopDependentEffects(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		set     func(*managementFake)
		want    string
		absence string
	}{
		{name: "preflight", set: func(fake *managementFake) { fake.preflight = false }, want: "preflight", absence: "create:Namespace"},
		{name: "helm", set: func(fake *managementFake) { fake.operatorErr = errors.New("helm failure") }, want: "helm", absence: "create:DPFOperatorConfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newManagementFake(t, generated)
			tc.set(client)
			journal := &executionJournal{file: &managementMemoryFile{}, digest: generated.Index().BundleDigest}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := runManagement(ctx, client, generated, journal, time.Millisecond)
			if err == nil {
				t.Fatal("expected management stage failure")
			}
			joined := strings.Join(client.events, ",")
			if !strings.Contains(joined, tc.want) || strings.Contains(joined, tc.absence) {
				t.Fatalf("expected %q and no %q after %s failure, events=%s", tc.want, tc.absence, tc.name, joined)
			}
		})
	}
}

func TestRunManagementReturnsOnlyBoundedBootstrapReceiptAfterReadyConfig(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newManagementFake(t, generated)
	journalFile := &managementMemoryFile{}
	journal := &executionJournal{file: journalFile, digest: generated.Index().BundleDigest}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	receipt, err := runManagement(ctx, client, generated, journal, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BundleDigest != generated.Index().BundleDigest || receipt.ManagerDeploymentUID != "manager-uid" || receipt.OperatorConfigUID != "config-uid" || len(receipt.NamespaceUIDs) != 2 {
		t.Fatalf("incomplete management bootstrap receipt: %#v", receipt)
	}
	if strings.Contains(fmt.Sprintf("%#v", receipt), "DPUDeployment") || len(client.events) == 0 || client.events[len(client.events)-1] != "verify" {
		t.Fatalf("receipt claimed later stages or omitted final identity verification: %#v events=%v", receipt, client.events)
	}
	if !strings.Contains(journalFile.String(), "DPF_CONFIG_READY") {
		t.Fatalf("readiness receipt missing from durable journal: %s", journalFile.String())
	}
}

func TestRunManagementAcceptsOperatorConfigControllerFinalizerAfterCreate(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
	client.configOnRead = func(config *unstructured.Unstructured) {
		config.SetFinalizers([]string{"dpu.nvidia.com/dpfoperatorconfig"})
		config.SetResourceVersion("2")
	}
	journalFile := &managementMemoryFile{}
	journal := &executionJournal{file: journalFile, digest: execution.plan.Digest()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	receipt, err := runPreparedManagement(ctx, client, execution, journal, time.Millisecond)
	if err != nil {
		t.Fatalf("normal controller finalizer rejected after a verified create: %v", err)
	}
	if receipt.OperatorConfigUID != "config-uid" || !strings.Contains(journalFile.String(), "DPF_CONFIG_READY") || client.createConfig != 1 {
		t.Fatal("controller finalizer transition did not produce one exact ready bootstrap receipt")
	}
	// Later tenant bootstrap uses this same full verifier, and existing-object
	// reconciliation must not turn the controller's finalizer into a second create.
	current, disposition, err := execution.reconciler.Ensure(ctx, client, execution.configObject)
	if err != nil || current == nil || disposition != DispositionExisting || client.createConfig != 1 {
		t.Fatalf("exact existing config with its controller finalizer was not preserved: %v", err)
	}
	tenantClient := newTenantStageFake(t, execution)
	tenantClient.resources[tenantStageKey(current)] = current.DeepCopy()
	if err := verifyManagementBootstrap(ctx, tenantClient, execution.verificationInputs(), tenantStageBootstrapReceipt(execution, tenantClient)); err != nil {
		t.Fatalf("tenant bootstrap rejected the same exact finalized operator config: %v", err)
	}
}

func TestRunManagementRechecksSingletonBeforeConfigCreateAndVerifiesVersion(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newManagementFake(t, generated)
	client.listHook = func(fake *managementFake) {
		if fake.listConfig == 2 && fake.config == nil {
			if !strings.Contains(strings.Join(fake.events, ","), "get:Deployment") {
				t.Fatal("singleton recheck happened before manager readiness")
			}
			fake.config = &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "operator.dpu.nvidia.com/v1alpha1", "kind": "DPFOperatorConfig",
				"metadata": map[string]any{"name": "foreign", "namespace": "dpf-operator-system", "uid": "foreign", "resourceVersion": "1"},
			}}
		}
	}
	journal := &executionJournal{file: &managementMemoryFile{}, digest: generated.Index().BundleDigest}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = runManagement(ctx, client, generated, journal, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "conflicting DPFOperatorConfig") || client.createConfig != 0 {
		t.Fatalf("late singleton collision error=%v, config creates=%d", err, client.createConfig)
	}

	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		set  func(*managementFake)
		code string
	}{
		{name: "create failure", set: func(fake *managementFake) { fake.configErr = errors.New("config create failed") }, code: "RESOURCE_CREATE_UNCERTAIN"},
		{name: "wrong version", set: func(fake *managementFake) { fake.targetVersion = "v26.4.0" }, code: "DPF_CONFIG_VERSION_MISMATCH"},
		{name: "namespace replaced", set: func(fake *managementFake) { fake.replaceNamespaceOnConfig = true }, code: "MANAGEMENT_NAMESPACE_CHANGED"},
		{name: "manager command drift", set: func(fake *managementFake) { fake.alterManagerOnConfig = true }, code: "DPF_MANAGER_CHANGED"},
		{name: "identity unavailable", set: func(fake *managementFake) { fake.failIdentityAfterConfig = true }, code: "MANAGEMENT_IDENTITY_UNVERIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
			tc.set(client)
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: generated.Index().BundleDigest}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := runPreparedManagement(ctx, client, execution, journal, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.code) || client.createConfig != 1 {
				t.Fatalf("config stage was not reached and safely gated: error=%v creates=%d", err, client.createConfig)
			}
			lines := bytes.Split(bytes.TrimSpace(file.Bytes()), []byte("\n"))
			var last executionEvent
			if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
				t.Fatal(err)
			}
			if last.ReasonCode != tc.code || (last.State != "failed" && last.State != "uncertain") {
				t.Fatalf("failure was not persisted as the terminal result: %#v", last)
			}
		})
	}
}

type managementFailingFile struct{}

func (managementFailingFile) Write([]byte) (int, error) { return 0, errors.New("report write failed") }
func (managementFailingFile) Sync() error               { return nil }
func (managementFailingFile) Close() error              { return nil }

func TestRunManagementJournalFailurePreventsManagementCalls(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newManagementFake(t, generated)
	journal := &executionJournal{file: managementFailingFile{}, digest: generated.Index().BundleDigest}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := runManagement(ctx, client, generated, journal, time.Millisecond); err == nil || len(client.events) != 0 {
		t.Fatalf("journal failure allowed management calls: err=%v events=%v", err, client.events)
	}
}

func TestManagerIntentRequiresExactRuntimeEntrypointAndContainerSet(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "manager"}},
		"template": map[string]any{"spec": map[string]any{"serviceAccountName": "manager", "containers": []any{
			map[string]any{"name": "manager", "image": "example/manager:v1", "args": []any{"--config-name=x", "--config-namespace=y"}},
		}}},
	}}}
	current := desired.DeepCopy()
	if !managerDeploymentIntentMatches(desired, current) {
		t.Fatal("identical manager deployment did not match")
	}
	mutations := []func(*unstructured.Unstructured){
		func(object *unstructured.Unstructured) { object.Object["spec"].(map[string]any)["replicas"] = int64(0) },
		func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"] = []any{"--config-name=wrong"}
		},
		func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["serviceAccountName"] = "other"
		},
		func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"] = append(object.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any), map[string]any{"name": "injected", "image": "attacker"})
		},
	}
	for index, mutate := range mutations {
		observed := desired.DeepCopy()
		mutate(observed)
		if managerDeploymentIntentMatches(desired, observed) {
			t.Fatalf("unsafe manager mutation %d was accepted", index)
		}
	}
}

func TestCRDSpecMustMatchRetainedSchema(t *testing.T) {
	spec := map[string]any{"group": "example.test", "scope": "Namespaced", "names": map[string]any{"plural": "things", "kind": "Thing"}, "versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}}}
	current := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "things.example.test"}, "spec": spec}}
	expected, err := operatorCRDFromObject(current)
	if err != nil {
		t.Fatal(err)
	}
	if !crdSpecMatches(expected.normalizedSpecJSON, current) {
		t.Fatal("same retained CRD spec did not match")
	}
	current = current.DeepCopy()
	_ = unstructured.SetNestedField(current.Object, "different.test", "spec", "group")
	if crdSpecMatches(expected.normalizedSpecJSON, current) {
		t.Fatal("changed CRD spec was accepted")
	}
}

func TestOperatorCRDReadinessAcceptsAPIDefaultedRetainedChart(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client := managementFakeWithAPIDefaultedCRDs(t, execution)
	if len(execution.crds) != 29 {
		t.Fatal("retained 26.8 chart does not contain the expected complete CRD set")
	}
	expectedBefore := make(map[string]any, len(execution.crds))
	currentBefore := make(map[string]*unstructured.Unstructured, len(client.crds))
	for _, expected := range execution.crds {
		expectedBefore[expected.name] = runtime.DeepCopyJSONValue(expected.spec)
		currentBefore[expected.name] = client.crds[expected.name].DeepCopy()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	file := &managementMemoryFile{}
	journal := &executionJournal{file: file, digest: execution.plan.Digest()}
	if err := waitOperatorCRDs(ctx, client, execution.crds, journal, time.Millisecond); err != nil {
		t.Fatalf("published CRDs with normal API defaults did not become ready: %v", err)
	}
	for _, expected := range execution.crds {
		if !crdIdentityMatches(expected, client.crds[expected.name]) {
			t.Fatalf("enrollment's shared full-spec guard rejects API-defaulted %s", expected.name)
		}
		if !reflect.DeepEqual(expected.spec, expectedBefore[expected.name]) ||
			!reflect.DeepEqual(client.crds[expected.name].Object, currentBefore[expected.name].Object) {
			t.Fatal("CRD comparison mutated retained or observed schema")
		}
	}
	if !strings.Contains(file.String(), "DPF_CRDS_ESTABLISHED") || client.createConfig != 0 {
		t.Fatal("CRD readiness omitted its receipt or performed a dependent config effect")
	}
}

func TestOperatorCRDReadinessRejectsSchemaConversionAndIdentityDrift(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client := managementFakeWithAPIDefaultedCRDs(t, execution)
	const name = "dpus.provisioning.dpu.nvidia.com"
	var expected operatorCRDExpectation
	for _, candidate := range execution.crds {
		if candidate.name == name {
			expected = candidate
		}
	}
	baseline := client.crds[name]
	if expected.name == "" || baseline == nil {
		t.Fatal("retained chart has no DPU webhook schema")
	}
	set := func(value any, path ...string) func(*unstructured.Unstructured) {
		return func(object *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(object.Object, value, path...); err != nil {
				t.Fatal(err)
			}
		}
	}
	version := func(value any, path ...string) func(*unstructured.Unstructured) {
		return func(object *unstructured.Unstructured) {
			versions, found, err := unstructured.NestedSlice(object.Object, "spec", "versions")
			if err != nil || !found || len(versions) == 0 {
				t.Fatal("retained DPU schema has no version")
			}
			first, ok := versions[0].(map[string]any)
			if !ok {
				t.Fatal("retained DPU version is malformed")
			}
			if err := unstructured.SetNestedField(first, value, path...); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedSlice(object.Object, versions, "spec", "versions"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		reason string
	}{
		{name: "schema pattern", mutate: version("changed", "schema", "openAPIV3Schema", "properties", "spec", "properties", "serialNumber", "pattern"), reason: "retained spec"},
		{name: "schema CEL", mutate: version([]any{map[string]any{"rule": "false"}}, "schema", "openAPIV3Schema", "x-kubernetes-validations"), reason: "retained spec"},
		{name: "version", mutate: version("v99", "name"), reason: "retained spec"},
		{name: "served", mutate: version(false, "served"), reason: "retained spec"},
		{name: "storage", mutate: version(false, "storage"), reason: "retained spec"},
		{name: "conversion strategy", mutate: set("None", "spec", "conversion", "strategy"), reason: "retained spec"},
		{name: "webhook name", mutate: set("untrusted-sentinel", "spec", "conversion", "webhook", "clientConfig", "service", "name"), reason: "retained spec"},
		{name: "webhook namespace", mutate: set("untrusted-sentinel", "spec", "conversion", "webhook", "clientConfig", "service", "namespace"), reason: "retained spec"},
		{name: "webhook path", mutate: set("/untrusted-sentinel", "spec", "conversion", "webhook", "clientConfig", "service", "path"), reason: "retained spec"},
		{name: "webhook CA", mutate: set("Y2hhbmdlZA==", "spec", "conversion", "webhook", "clientConfig", "caBundle"), reason: "retained spec"},
		{name: "webhook port", mutate: set(int64(8443), "spec", "conversion", "webhook", "clientConfig", "service", "port"), reason: "retained spec"},
		{name: "webhook explicit zero port", mutate: set(int64(0), "spec", "conversion", "webhook", "clientConfig", "service", "port"), reason: "retained spec"},
		{name: "scope", mutate: set("Cluster", "spec", "scope"), reason: "scope"},
		{name: "missing UID", mutate: func(object *unstructured.Unstructured) { object.SetUID("") }, reason: "server metadata"},
		{name: "missing resource version", mutate: func(object *unstructured.Unstructured) { object.SetResourceVersion("") }, reason: "server metadata"},
		{name: "wrong identity", mutate: func(object *unstructured.Unstructured) { object.SetName("untrusted-sentinel") }, reason: "identity"},
		{name: "deleting", mutate: func(object *unstructured.Unstructured) { now := metav1.Now(); object.SetDeletionTimestamp(&now) }, reason: "identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := baseline.DeepCopy()
			test.mutate(current)
			client.crds[name] = current
			if crdIdentityMatches(expected, current) {
				t.Fatal("enrollment's shared full-spec guard accepted drift")
			}
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: execution.plan.Digest()}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := waitOperatorCRDs(ctx, client, []operatorCRDExpectation{expected}, journal, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "DPF_CRD_READINESS_FAILED") || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("CRD drift did not identify its retained definition and safe reason: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(file.String()), "\n")
			var event executionEvent
			if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &event) != nil || event.Subject != name ||
				event.ReasonCode != "DPF_CRD_READINESS_FAILED" || strings.Contains(file.String(), "untrusted-sentinel") || client.createConfig != 0 {
				t.Fatal("CRD failure journal lost identity, exposed remote values or allowed a dependent effect")
			}
		})
	}
	// Explicit nondefault intent stays authoritative rather than being rewritten to443.
	wanted := baseline.DeepCopy()
	set(int64(8443), "spec", "conversion", "webhook", "clientConfig", "service", "port")(wanted)
	wantedExpectation, err := operatorCRDFromObject(wanted)
	if err != nil {
		t.Fatal(err)
	}
	if !crdSpecMatches(wantedExpectation.normalizedSpecJSON, wanted) || crdSpecMatches(wantedExpectation.normalizedSpecJSON, baseline) {
		t.Fatal("covering defaults replaced or ignored an explicitly retained nondefault webhook port")
	}
}

func managementFakeWithAPIDefaultedCRDs(t *testing.T, execution *managementExecution) *managementFake {
	t.Helper()
	client := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
	for name, crd := range client.crds {
		// Model the API response independently of the comparator: these are the
		// exact standard defaults observed on the published 26.8 definitions.
		if name == "dpus.provisioning.dpu.nvidia.com" {
			if _, found, err := unstructured.NestedFieldNoCopy(crd.Object, "spec", "conversion", "webhook", "clientConfig", "service", "port"); err != nil || found {
				t.Fatal("published DPU CRD no longer omits its webhook service port")
			}
			if err := unstructured.SetNestedField(crd.Object, int64(443), "spec", "conversion", "webhook", "clientConfig", "service", "port"); err != nil {
				t.Fatal(err)
			}
		} else if err := unstructured.SetNestedMap(crd.Object, map[string]any{"strategy": "None"}, "spec", "conversion"); err != nil {
			t.Fatal(err)
		}
	}
	return client
}

func TestManagementJournalIsJSONLines(t *testing.T) {
	file := &managementMemoryFile{}
	journal := &executionJournal{file: file, digest: "sha256:test"}
	if err := journal.record(executionEvent{Stage: "test", Operation: "observe", State: "started", ReasonCode: "TEST", Message: "safe"}); err != nil {
		t.Fatal(err)
	}
	var event executionEvent
	if err := json.Unmarshal(bytes.TrimSpace(file.Bytes()), &event); err != nil || event.Stage != "test" {
		t.Fatalf("journal event did not encode: %#v err=%v", event, err)
	}
}

func TestDecodeOperatorManifestPreservesKubernetesIntegers(t *testing.T) {
	objects, err := decodeOperatorManifest([]byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 2\n"))
	if err != nil || len(objects) != 1 {
		t.Fatalf("decode: objects=%d err=%v", len(objects), err)
	}
	count, found, err := unstructured.NestedInt64(objects[0].Object, "spec", "replicas")
	if err != nil || !found || count != 2 {
		t.Fatalf("replica integer lost: count=%d found=%v err=%v", count, found, err)
	}
}

func TestOperatorConfigIntentMismatchMessageUsesOnlyFixedCategories(t *testing.T) {
	const sentinel = "untrusted-sentinel"
	var typedNil *ReconciliationError
	for _, tc := range []struct {
		name     string
		err      error
		category string
	}{
		{name: "known category", err: &ReconciliationError{ReasonCode: "RESOURCE_METADATA_CONFLICT", Message: sentinel, cause: errors.New(sentinel)}, category: "RESOURCE_METADATA_CONFLICT"},
		{name: "unknown category", err: &ReconciliationError{ReasonCode: sentinel, Message: sentinel, cause: errors.New(sentinel)}, category: "unavailable"},
		{name: "plain error", err: errors.New(sentinel), category: "unavailable"},
		{name: "typed nil", err: typedNil, category: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := operatorConfigIntentMismatchMessage(tc.err)
			if strings.Contains(message, sentinel) || !strings.Contains(message, tc.category) {
				t.Fatalf("unsafe or unclassified diagnostic: %q", message)
			}
		})
	}
}

func TestOperatorConfigFinalizerDoesNotRelaxReadinessOwnershipOrIntent(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := execution.reconciler.markedDesired(execution.configObject)
	if err != nil {
		t.Fatal(err)
	}
	baseline.SetUID("config-uid")
	baseline.SetResourceVersion("1")
	baseline.SetFinalizers([]string{"dpu.nvidia.com/dpfoperatorconfig"})
	for _, tc := range []struct {
		name         string
		mutate       func(*unstructured.Unstructured)
		category     string
		ensureReject bool
	}{
		{name: "unknown finalizer", mutate: func(o *unstructured.Unstructured) { o.SetFinalizers([]string{"untrusted-sentinel"}) }, category: "RESOURCE_METADATA_CONFLICT", ensureReject: true},
		{name: "duplicate finalizer", mutate: func(o *unstructured.Unstructured) {
			o.SetFinalizers([]string{"dpu.nvidia.com/dpfoperatorconfig", "dpu.nvidia.com/dpfoperatorconfig"})
		}, category: "RESOURCE_METADATA_CONFLICT", ensureReject: true},
		{name: "bundle marker", mutate: func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[BundleDigestAnnotation] = "untrusted-sentinel"
			o.SetAnnotations(a)
		}, category: "RESOURCE_OWNERSHIP_CONFLICT", ensureReject: true},
		{name: "object marker", mutate: func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[IntentDigestAnnotation] = "untrusted-sentinel"
			o.SetAnnotations(a)
		}, category: "RESOURCE_OWNERSHIP_CONFLICT", ensureReject: true},
		{name: "owner reference", mutate: func(o *unstructured.Unstructured) {
			o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: "untrusted-sentinel", UID: "untrusted-sentinel"}})
		}, category: "RESOURCE_METADATA_CONFLICT", ensureReject: true},
		{name: "spec drift", mutate: func(o *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(o.Object, int64(9000), "spec", "networking", "highSpeedMTU"); err != nil {
				t.Fatal(err)
			}
		}, category: "RESOURCE_CONTENT_CONFLICT", ensureReject: true},
		{name: "unknown spec field", mutate: func(o *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(o.Object, "untrusted-sentinel", "spec", "untrusted-sentinel"); err != nil {
				t.Fatal(err)
			}
		}, category: "RESOURCE_SCHEMA_INVALID", ensureReject: true},
		{name: "extra annotation", mutate: func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a["untrusted-sentinel"] = "untrusted-sentinel"
			o.SetAnnotations(a)
		}, category: "RESOURCE_METADATA_CONFLICT", ensureReject: true},
		{name: "extra label", mutate: func(o *unstructured.Unstructured) {
			o.SetLabels(map[string]string{"untrusted-sentinel": "untrusted-sentinel"})
		}, category: "RESOURCE_METADATA_CONFLICT", ensureReject: true},
		{name: "UID replacement", mutate: func(o *unstructured.Unstructured) { o.SetUID("untrusted-sentinel") }, category: "identity or retained intent", ensureReject: false},
		{name: "missing resource version", mutate: func(o *unstructured.Unstructured) { o.SetResourceVersion("") }, category: "identity or retained intent", ensureReject: true},
		{name: "deleting", mutate: func(o *unstructured.Unstructured) { now := metav1.Now(); o.SetDeletionTimestamp(&now) }, category: "identity or retained intent", ensureReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
			client.config = baseline.DeepCopy()
			tc.mutate(client.config)
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: execution.plan.Digest()}
			current, waitErr := waitOperatorConfigReady(ctx, client, execution.reconciler, execution.configObject, baseline, execution.operatorVersion, journal, time.Millisecond)
			if waitErr == nil || current != nil || !strings.Contains(waitErr.Error(), "DPF_CONFIG_INTENT_CHANGED") {
				t.Fatalf("modified finalized config passed readiness: %v", waitErr)
			}
			lines := strings.Split(strings.TrimSpace(file.String()), "\n")
			var last executionEvent
			if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &last) != nil || last.State != "failed" || last.ReasonCode != "DPF_CONFIG_INTENT_CHANGED" || !strings.Contains(last.Message, tc.category) || strings.Contains(file.String(), "untrusted-sentinel") || strings.Contains(file.String(), "DPF_CONFIG_READY") {
				t.Fatalf("unsafe or nonspecific failure journal: %s", file.String())
			}
			if tc.ensureReject {
				if _, _, err := execution.reconciler.Ensure(ctx, client, execution.configObject); err == nil {
					t.Fatal("modified existing config passed reconciliation")
				}
			}
			tenantClient := newTenantStageFake(t, execution)
			tenantClient.resources[tenantStageKey(client.config)] = client.config.DeepCopy()
			if err := verifyManagementBootstrap(ctx, tenantClient, execution.verificationInputs(), tenantStageBootstrapReceipt(execution, tenantClient)); err == nil {
				t.Fatal("modified config passed tenant bootstrap")
			}
			if client.createConfig != 0 || tenantClient.createCount != 0 {
				t.Fatal("failure allowed dependent effects")
			}
		})
	}
}

func TestManagementReportsExistingReleaseConflictWithoutInstallUncertainty(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "untrusted-sentinel"
	for _, tc := range []struct {
		name      string
		branch    string
		category  string
		uncertain bool
	}{
		{name: "actual existing values conflict", branch: "existing-values", category: "HELM_VALUES_DIFFER", uncertain: false},
		{name: "actual existing manifest conflict", branch: "existing-manifest", category: "HELM_MANIFEST_DIFFER", uncertain: false},
		{name: "actual install transport error", branch: "install-error", category: "DPF_OPERATOR_INSTALL_UNCERTAIN", uncertain: true},
		{name: "actual postinstall verification error", branch: "postinstall", category: "DPF_OPERATOR_INSTALL_UNCERTAIN", uncertain: true},
		{name: "unknown typed category", branch: "unknown", category: "HELM_RELEASE_UNVERIFIED", uncertain: false},
		{name: "wrapped typed category", branch: "wrapped", category: "HELM_VALUES_DIFFER", uncertain: false},
		{name: "untyped remote error", branch: "remote", category: "DPF_OPERATOR_INSTALL_UNCERTAIN", uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			installed := releaseForPlan(t, execution.operator)
			actions := &fakeOperatorActions{installed: installed}
			switch tc.branch {
			case "existing-values", "wrapped":
				installed.Config[sentinel] = true
			case "existing-manifest":
				installed.Manifest = strings.Replace(installed.Manifest, "name:", "name: "+sentinel+"-", 1)
			case "install-error":
				actions.getErr = driver.ErrReleaseNotFound
				actions.installErr = errors.New(sentinel)
			case "postinstall":
				actions.getErr = driver.ErrReleaseNotFound
				installed.Name = "foreign"
				actions.installRelease = installed
			}
			var operationErr error
			switch tc.branch {
			case "unknown":
				operationErr = &existingOperatorReleaseConflict{reason: operatorReleaseMismatch(sentinel)}
			case "remote":
				operationErr = errors.New(sentinel)
			default:
				operationErr = ensureOperator(ctx, execution.operator, actions)
			}
			if tc.branch == "wrapped" {
				operationErr = fmt.Errorf("%s: %w", sentinel, operationErr)
			}
			if operationErr == nil {
				t.Fatal("real operator branch did not produce its expected conflict/failure")
			}
			if !tc.uncertain && tc.branch != "unknown" && (len(actions.installs) != 0 || actions.resourceChecks != 0) {
				t.Fatal("existing release verification reached install/adoption")
			}
			if (tc.branch == "install-error" || tc.branch == "postinstall") && len(actions.installs) != 1 {
				t.Fatal("uncertain case did not dispatch install")
			}
			client := managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds)
			client.operatorErr = operationErr
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: execution.plan.Digest()}
			_, err := runPreparedManagement(ctx, client, execution, journal, time.Millisecond)
			if err == nil || strings.Contains(err.Error(), sentinel) || strings.Contains(file.String(), sentinel) || client.createConfig != 0 {
				t.Fatal("operator failure leaked private content or allowed Config creation")
			}
			lines := strings.Split(strings.TrimSpace(file.String()), "\n")
			if len(lines) == 0 {
				t.Fatal("missing durable journal")
			}
			var event executionEvent
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil {
				t.Fatal(err)
			}
			wantState, wantCode := "failed", "DPF_OPERATOR_RELEASE_CONFLICT"
			if tc.uncertain {
				wantState, wantCode = "uncertain", "DPF_OPERATOR_INSTALL_UNCERTAIN"
			}
			if event.State != wantState || event.ReasonCode != wantCode || !strings.Contains(event.Message, tc.category) && !tc.uncertain {
				t.Fatalf("incorrect operator failure provenance: %#v", event)
			}
			if !tc.uncertain && !strings.Contains(event.Message, "no Helm install or adoption was attempted") {
				t.Fatal("Get-only conflict omitted its effect boundary")
			}
		})
	}
}
