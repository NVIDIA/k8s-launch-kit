// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

type tenantSourceStub struct {
	sources tenantSources
	calls   int
	mutate  func(int, *tenantSources)
}

func (s *tenantSourceStub) observeTenantSources(ctx context.Context, _ controllercontract.TenantReadinessExpected) (tenantSources, error) {
	if err := ctx.Err(); err != nil {
		return tenantSources{}, err
	}
	s.calls++
	if s.mutate != nil {
		s.mutate(s.calls, &s.sources)
	}
	return s.sources, nil
}

func tenantSourceFixture(t *testing.T) (controllercontract.TenantReadinessExpected, tenantSources) {
	t.Helper()
	expected, observed := readyTenantFixture(t)
	if err := unstructured.SetNestedField(observed.DPUCluster.Object, "v"+strings.TrimPrefix(expected.KubernetesVersion, "v"), "status", "version"); err != nil {
		t.Fatal(err)
	}
	data, err := clientcmd.Write(tenantKubeconfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: expected.Name + "-admin-kubeconfig", Namespace: expected.Namespace, UID: types.UID("secret-uid"), ResourceVersion: "1",
	}, Data: map[string][]byte{"super-admin.conf": data}}
	return expected, tenantSources{observed: observed, secret: secret}
}

func TestTenantSourcesBindCredentialsAndResourceIdentity(t *testing.T) {
	expected, sources := tenantSourceFixture(t)
	original, endpoint, data, err := validateTenantSources(testRules(t), expected, "dc-uid", sources)
	if err != nil || endpoint != "https://192.0.2.20:30538" || len(data) == 0 {
		t.Fatalf("unexpected binding: endpoint=%s err=%v", endpoint, err)
	}
	sources.secret.ResourceVersion = "2"
	sources.observed.DPUCluster.SetResourceVersion("3")
	unchanged, _, _, err := validateTenantSources(testRules(t), expected, "dc-uid", sources)
	if err != nil || unchanged != original {
		t.Fatal("metadata-only updates invalidated source identity")
	}
	sources.secret.Data["super-admin.conf"] = append(sources.secret.Data["super-admin.conf"], '\n')
	changed, _, _, err := validateTenantSources(testRules(t), expected, "dc-uid", sources)
	if err != nil || changed == original {
		t.Fatal("credential byte change was not bound")
	}
}

func TestTenantSourcesRejectDriftAndMalformedCredentials(t *testing.T) {
	for name, mutate := range map[string]func(*tenantSources){
		"cluster replacement": func(s *tenantSources) { s.observed.DPUCluster.SetUID("replacement") },
		"secret deletion":     func(s *tenantSources) { now := metav1.Now(); s.secret.DeletionTimestamp = &now },
		"secret namespace":    func(s *tenantSources) { s.secret.Namespace = "another" },
		"missing key":         func(s *tenantSources) { s.secret.Data = map[string][]byte{"admin.conf": []byte("secret-canary")} },
		"too large":           func(s *tenantSources) { s.secret.Data["super-admin.conf"] = make([]byte, (1<<20)+1) },
		"conflicting owner": func(s *tenantSources) {
			s.secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kamaji.clastix.io/v1alpha1", Kind: "TenantControlPlane", Name: s.secret.Name, UID: "other"}}
		},
		"cluster version": func(s *tenantSources) {
			_ = unstructured.SetNestedField(s.observed.DPUCluster.Object, "v0.0.1", "status", "version")
		},
	} {
		t.Run(name, func(t *testing.T) {
			expected, sources := tenantSourceFixture(t)
			mutate(&sources)
			_, _, _, err := validateTenantSources(testRules(t), expected, "dc-uid", sources)
			if err == nil {
				t.Fatal("invalid source accepted")
			}
			if strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func tenantIdentityClientFixture(t *testing.T, expected controllercontract.TenantReadinessExpected, source *tenantSourceStub) (*tenantClient, *int) {
	t.Helper()
	return tenantIdentityClientFixtureWithHandler(t, expected, source, nil)
}

func tenantIdentityClientFixtureWithHandler(t *testing.T, expected controllercontract.TenantReadinessExpected, source *tenantSourceStub, handle func(http.ResponseWriter, *http.Request) bool) (*tenantClient, *int) {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if handle != nil && handle(w, r) {
			return
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v" + strings.TrimPrefix(expected.KubernetesVersion, "v")})
		default:
			t.Errorf("unexpected tenant request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	digest, _, _, err := validateTenantSources(testRules(t), expected, "dc-uid", source.sources)
	if err != nil {
		t.Fatal(err)
	}
	return &tenantClient{rules: testRules(t), source: source, expected: expected, dpuClusterUID: "dc-uid", sourceDigest: digest, systemUID: "tenant-system", api: api}, &requests
}

func TestTenantIdentityChecksSourceBeforeAndAfterAPIReads(t *testing.T) {
	for _, when := range []int{0, 1, 2} {
		t.Run(string(rune('0'+when)), func(t *testing.T) {
			expected, sources := tenantSourceFixture(t)
			source := &tenantSourceStub{sources: sources, mutate: func(call int, s *tenantSources) {
				if call == when {
					s.secret.UID = "replaced-secret"
				}
			}}
			client, requests := tenantIdentityClientFixture(t, expected, source)
			err := client.VerifyIdentity(context.Background())
			if when == 0 && err != nil {
				t.Fatal(err)
			}
			if when != 0 && err == nil {
				t.Fatal("source replacement accepted")
			}
			if when == 1 && *requests != 0 {
				t.Fatal("tenant contacted after precheck failure")
			}
			if when == 2 && *requests != 2 {
				t.Fatal("fixture did not exercise post-request source check")
			}
		})
	}
}

func TestTenantIdentityRejectsReplacedTenantAndCancellation(t *testing.T) {
	expected, sources := tenantSourceFixture(t)
	source := &tenantSourceStub{sources: sources}
	client, _ := tenantIdentityClientFixture(t, expected, source)
	client.systemUID = "former-tenant"
	if err := client.VerifyIdentity(context.Background()); err == nil {
		t.Fatal("replaced tenant accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.VerifyIdentity(canceled); err == nil {
		t.Fatal("cancellation accepted")
	}
}

func TestTenantClientRejectsIncompleteBindingBeforeConnecting(t *testing.T) {
	expected, sources := tenantSourceFixture(t)
	source := &tenantSourceStub{sources: sources}
	if _, err := newTenantClient(context.Background(), testRules(t), source, expected, "", time.Second, nil, nil); err == nil || source.calls != 0 {
		t.Fatal("incomplete binding accessed management sources")
	}
	source.sources.secret.Data = map[string][]byte{"admin.conf": []byte("secret-canary")}
	if _, err := newTenantClient(context.Background(), testRules(t), source, expected, "dc-uid", time.Second, nil, nil); err == nil || strings.Contains(err.Error(), "secret-canary") {
		t.Fatal("invalid credential source accepted or leaked")
	}
}

func TestTenantSourcesMeasuredReplacementPrecedesReadiness(t *testing.T) {
	for _, state := range []string{"old-child-owner", "pending", "deleting", "unready-same-uid", "absent-uid", "absent-cluster"} {
		t.Run(state, func(t *testing.T) {
			expected, sources := tenantSourceFixture(t)
			replaced := state == "old-child-owner" || state == "pending" || state == "deleting"
			if replaced {
				sources.observed.DPUCluster.SetUID("replacement")
			}
			switch state {
			case "pending", "unready-same-uid":
				unstructured.RemoveNestedField(sources.observed.DPUCluster.Object, "status", "conditions")
			case "deleting":
				now := metav1.Now()
				sources.observed.DPUCluster.SetDeletionTimestamp(&now)
			case "absent-uid":
				sources.observed.DPUCluster.SetUID("")
			case "absent-cluster":
				sources.observed.DPUCluster = nil
			}
			_, _, _, err := validateTenantSources(testRules(t), expected, "dc-uid", sources)
			if err == nil || errors.Is(err, errTenantIdentityChanged) != replaced {
				t.Fatalf("replacement=%v err=%v", replaced, err)
			}
		})
	}
}
