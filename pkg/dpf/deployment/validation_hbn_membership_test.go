// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// This server honors Kubernetes label selectors, unlike the original runtime
// fixture. Extra Pods are placed on the second page, after the healthy Pod.
type hbnMembershipHTTPState struct {
	mu                        sync.Mutex
	daemonSets                []appsv1.DaemonSet
	pods                      []corev1.Pod
	failKind, failure         string
	selectors                 []string
	permissionReads, podReads int
}

func hbnMembershipHTTPObserver(t *testing.T) (func() (validationHBNBindings, error), *hbnMembershipHTTPState) {
	t.Helper()
	f := newRuntimeStageFixture(t, []string{"SERIAL-A"})
	admitN3cHBNPods(f)
	execution := f.prepared.execution
	desired, err := prepareManagementDesiredFromPlan(execution.retained, execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	activation, err := observeActivationScope(f.ctx, f.client, execution.verificationInputs(), wanted, f.activation.DeploymentUID, f.activation.selected, f.activation.progress)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := resolveHostWorkloadLeases(execution.plan, execution.reconciler, f.activation.selected, activation, f.workload.BundleDigest, f.workload.Resources, f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	graph := currentRuntimeGraph{deployment: activation.deployment, services: []unstructured.Unstructured{*f.client.service.DeepCopy()}, interfaces: deepCopyUnstructuredList(f.client.interfaces)}
	n3cReadyWorkload(&graph.services[0])
	management := &correctionHBNManagement{validationReadFake: &validationReadFake{}, app: f.client.app}
	tenant := testTenant(f.controlPlane.tenant.Client)
	state := &hbnMembershipHTTPState{daemonSets: []appsv1.DaemonSet{*f.ds.DeepCopy()}, pods: []corev1.Pod{*f.pods[0].DeepCopy()}}
	namespace := f.ds.Namespace
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" && r.Method == http.MethodPost {
			state.permissionReads++
			_ = json.NewEncoder(w).Encode(&authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}})
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("prohibited HBN membership action %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		kind := ""
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/" + namespace + "/daemonsets":
			kind = "daemonsets"
		case "/api/v1/namespaces/" + namespace + "/pods":
			kind = "pods"
		}
		if kind != "" {
			state.selectors = append(state.selectors, r.URL.Query().Get("labelSelector"))
			if r.URL.Query().Get("limit") != strconv.Itoa(hbnRuntimePageSize) {
				t.Errorf("membership list is not bounded: %s", r.URL.RawQuery)
			}
			selector, parseErr := labels.Parse(r.URL.Query().Get("labelSelector"))
			if parseErr != nil {
				t.Errorf("invalid membership selector: %v", parseErr)
			}
			meta := metav1.ListMeta{ResourceVersion: "10"}
			if kind == state.failKind {
				switch state.failure {
				case "denied":
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(&metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden})
					return
				case "missing resource version":
					meta.ResourceVersion = ""
				case "repeated continuation":
					meta.Continue = "repeat"
				}
			}
			if kind == "daemonsets" {
				items := []appsv1.DaemonSet{}
				for _, ds := range state.daemonSets {
					if selector.Matches(labels.Set(ds.Labels)) {
						items = append(items, ds)
					}
				}
				_ = json.NewEncoder(w).Encode(&appsv1.DaemonSetList{ListMeta: meta, Items: items})
				return
			}
			items := []corev1.Pod{}
			for _, pod := range state.pods {
				if selector.Matches(labels.Set(pod.Labels)) {
					items = append(items, pod)
				}
			}
			if len(items) > 1 {
				if r.URL.Query().Get("continue") == "" {
					items, meta.Continue = items[:1], "second"
				} else {
					items = items[1:]
				}
			}
			_ = json.NewEncoder(w).Encode(&corev1.PodList{ListMeta: meta, Items: items})
			return
		}
		for _, pod := range state.pods {
			if r.URL.Path == "/api/v1/namespaces/"+namespace+"/pods/"+pod.Name {
				state.podReads++
				_ = json.NewEncoder(w).Encode(pod)
				return
			}
		}
		t.Errorf("prohibited HBN membership read %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	tenant.api, err = kubernetes.NewForConfig(&rest.Config{Host: server.URL, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	return func() (validationHBNBindings, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return observeCurrentHBNBindings(ctx, management, tenant.currentValidationReads(), desired, graph, f.snapshot, leases, f.image, true)
	}, state
}

func TestValidationHBNCompleteMembershipRejectsHiddenParticipants(t *testing.T) {
	observe, state := hbnMembershipHTTPObserver(t)
	baseline, err := observe()
	if err != nil || len(baseline.anchors) != 1 {
		t.Fatalf("healthy native HTTP-backed baseline: anchors=%d err=%v", len(baseline.anchors), err)
	}
	selectedDS, selectedPod := state.daemonSets[0], state.pods[0]
	for _, test := range []struct {
		name      string
		label     string
		node      string
		extraDS   bool
		wantState validation.State
	}{
		{name: "unlabelled owned duplicate", node: selectedPod.Spec.NodeName, wantState: validation.Pending},
		{name: "mislabelled owned duplicate", label: "another-service", node: selectedPod.Spec.NodeName, wantState: validation.Pending},
		{name: "unlabelled owned unselected node", node: "unselected-node", wantState: validation.Failed},
		{name: "mislabelled owned unselected node", label: "another-service", node: "unselected-node", wantState: validation.Failed},
		{name: "extra DS selected by template only", extraDS: true, wantState: validation.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			state.mu.Lock()
			state.daemonSets, state.pods = []appsv1.DaemonSet{selectedDS}, []corev1.Pod{selectedPod}
			if test.extraDS {
				extra := selectedDS.DeepCopy()
				extra.Name, extra.UID = "template-only-extra", "extra-ds"
				delete(extra.Labels, hbnServiceIDLabel)
				state.daemonSets = append(state.daemonSets, *extra)
			} else {
				extra := selectedPod.DeepCopy()
				extra.Name, extra.UID, extra.Spec.NodeName = "hidden-owned-pod", "extra-pod", test.node
				delete(extra.Labels, hbnServiceIDLabel)
				if test.label != "" {
					extra.Labels[hbnServiceIDLabel] = test.label
				}
				state.pods = append(state.pods, *extra)
			}
			state.permissionReads, state.podReads, state.selectors = 0, 0, nil
			state.mu.Unlock()
			bindings, readErr := observe()
			var check *validationCheckError
			if !errors.As(readErr, &check) || check.state != test.wantState || len(bindings.anchors) != 0 {
				t.Fatalf("hidden participant accepted or misclassified: anchors=%d err=%v check=%+v", len(bindings.anchors), readErr, check)
			}
			// A newly appearing participant must block the next diagnostic at the
			// existing session fence, before its command function can execute.
			reads := 0
			session := &validationRuntimeSession{anchor: validationCurrentCohort{bindings: baseline}, observe: func(context.Context) (validationCurrentCohort, error) {
				current, captureErr := observe()
				return validationCurrentCohort{bindings: current}, captureErr
			}, read: func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
				reads++
				return nil, nil
			}}
			if _, probeErr := session.ObserveBGP(context.Background(), "SERIAL-A"); probeErr == nil || reads != 0 {
				t.Fatalf("incomplete membership authorized diagnostic: reads=%d err=%v", reads, probeErr)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.permissionReads != 0 || state.podReads != 0 {
				t.Fatalf("membership rejection reached dependent reads: permissions=%d named Pods=%d", state.permissionReads, state.podReads)
			}
			for _, selector := range state.selectors {
				if selector != "" {
					t.Fatalf("membership used incomplete label-filtered scope %q", selector)
				}
			}
		})
	}
}

func TestValidationHBNCompleteMembershipIgnoresUnrelatedWorkloads(t *testing.T) {
	observe, state := hbnMembershipHTTPObserver(t)
	baseline, err := observe()
	if err != nil || len(baseline.anchors) != 1 {
		t.Fatalf("healthy baseline: %v", err)
	}
	state.mu.Lock()
	ds := state.daemonSets[0].DeepCopy()
	ds.Name, ds.UID = "another-service", "another-ds"
	ds.Labels[hbnServiceIDLabel], ds.Spec.Template.Labels[hbnServiceIDLabel] = "another-service", "another-service"
	pod := state.pods[0].DeepCopy()
	pod.Name, pod.UID, pod.Spec.NodeName = "another-service-pod", "another-pod", "unselected-node"
	pod.Labels[hbnServiceIDLabel] = "another-service"
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = ds.Name, ds.UID
	unowned := pod.DeepCopy()
	unowned.Name, unowned.UID, unowned.Labels, unowned.OwnerReferences = "unlabelled-unowned", "unowned-pod", nil, nil
	state.daemonSets = append(state.daemonSets, *ds)
	state.pods = append(state.pods, *pod, *unowned)
	state.mu.Unlock()
	current, err := observe()
	if err != nil || !reflect.DeepEqual(baseline, current) {
		t.Fatalf("unrelated namespace objects changed selected source fence: err=%v", err)
	}
}

func TestValidationHBNCompleteMembershipIncompleteListsCannotBind(t *testing.T) {
	observe, state := hbnMembershipHTTPObserver(t)
	for _, kind := range []string{"daemonsets", "pods"} {
		for _, failure := range []string{"denied", "missing resource version", "repeated continuation"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				state.mu.Lock()
				state.failKind, state.failure, state.permissionReads, state.podReads = kind, failure, 0, 0
				state.mu.Unlock()
				bindings, err := observe()
				if err == nil || len(bindings.anchors) != 0 {
					t.Fatalf("incomplete list accepted: anchors=%d err=%v", len(bindings.anchors), err)
				}
				if resultState, _, _, _ := validationErrorFields(err); resultState != validation.Unverified {
					t.Fatalf("incomplete list did not remain unverified: state=%s err=%v", resultState, err)
				}
				if failure == "denied" && validation.ErrorCategory(validationRuntimeStageError(context.Background(), validation.StageRuntimeHBN, err)) != validation.FatalAccess {
					t.Fatalf("denied list lost access classification: %v", err)
				}
				if failure != "denied" && !strings.Contains(err.Error(), "HBN_RUNTIME_LIST_INCOMPLETE") {
					t.Fatalf("incomplete pagination lost conservative classification: %v", err)
				}
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.permissionReads != 0 || state.podReads != 0 {
					t.Fatalf("incomplete list reached dependent reads: permissions=%d named Pods=%d", state.permissionReads, state.podReads)
				}
			})
		}
	}
}
