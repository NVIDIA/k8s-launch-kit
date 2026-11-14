// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
			"spec":     map[string]any{},
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Established", "status": "True"}, map[string]any{"type": "NamesAccepted", "status": "True"},
			}},
		}}
		crd.Object["spec"] = expected.spec
		crds[expected.name] = crd
		for _, version := range expected.versions {
			gv := expected.group + "/" + version
			resources[gv] = append(resources[gv], metav1.APIResource{Name: expected.plural, Kind: expected.kind, Namespaced: expected.namespaced})
		}
	}
	return &managementFake{bundleDigest: plan.Digest(), manager: manager, crds: crds,
		resources: resources, namespaces: map[string]*unstructured.Unstructured{}, preflight: true, binding: true}
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

func (f *managementFake) CheckManagementPreflight(context.Context, ManagementPreflightRequest) PreflightResult {
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
	generated, err := generation.Generate(preflightGenerationInputs(t))
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
	generated, err := generation.Generate(preflightGenerationInputs(t))
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
	generated, err := generation.Generate(preflightGenerationInputs(t))
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

func TestRunManagementRechecksSingletonBeforeConfigCreateAndVerifiesVersion(t *testing.T) {
	generated, err := generation.Generate(preflightGenerationInputs(t))
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

	for _, tc := range []struct {
		name string
		set  func(*managementFake)
		code string
	}{
		{name: "create failure", set: func(fake *managementFake) { fake.configErr = errors.New("config create failed") }, code: "RESOURCE_CREATE_UNCERTAIN"},
		{name: "wrong version", set: func(fake *managementFake) { fake.targetVersion = "v26.4.0" }, code: "DPF_CONFIG_VERSION_MISMATCH"},
		{name: "namespace replaced", set: func(fake *managementFake) { fake.replaceNamespaceOnConfig = true }, code: "MANAGEMENT_NAMESPACE_CHANGED"},
		{name: "manager command drift", set: func(fake *managementFake) { fake.alterManagerOnConfig = true }, code: "DPF_MANAGER_CHANGED"},
		{name: "identity changed", set: func(fake *managementFake) { fake.failIdentityAfterConfig = true }, code: "MANAGEMENT_IDENTITY_CHANGED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newManagementFake(t, generated)
			tc.set(client)
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: generated.Index().BundleDigest}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := runManagement(ctx, client, generated, journal, time.Millisecond)
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
	generated, err := generation.Generate(preflightGenerationInputs(t))
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
	expected := map[string]any{"group": "example.test", "scope": "Namespaced", "names": map[string]any{"plural": "things", "kind": "Thing"}, "versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}}}
	current := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "things.example.test"}, "spec": expected}}
	if !crdSpecMatches(expected, current) {
		t.Fatal("same retained CRD spec did not match")
	}
	current = current.DeepCopy()
	_ = unstructured.SetNestedField(current.Object, "different.test", "spec", "group")
	if crdSpecMatches(expected, current) {
		t.Fatal("changed CRD spec was accepted")
	}
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
