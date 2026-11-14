// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

type apiFixture struct {
	request       config.Request
	expected      generation.ManagementIdentity
	uid           atomic.Value
	writes        atomic.Int32
	calls         atomic.Int32
	stallIdentity atomic.Bool
}

func newAPIFixture(t *testing.T, handle http.HandlerFunc) *apiFixture {
	t.Helper()
	f := &apiFixture{}
	f.uid.Store("cluster-one")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.stallIdentity.Load() && strings.HasSuffix(r.URL.Path, "/namespaces/kube-system") {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			f.writes.Add(1)
		}
		switch r.URL.Path {
		case "/prefix/api/v1/namespaces/kube-system":
			fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":%q}}`, f.uid.Load())
		case "/prefix/version":
			fmt.Fprint(w, `{"gitVersion":"v1.35.6"}`)
		case "/prefix/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"configmaps","kind":"ConfigMap","namespaced":true,"verbs":["get","list","create"]}]}`)
		default:
			handle(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	raw := clientapi.Config{
		CurrentContext: "selected",
		Clusters:       map[string]*clientapi.Cluster{"m": {Server: server.URL + "/prefix", CertificateAuthorityData: ca}},
		AuthInfos:      map[string]*clientapi.AuthInfo{"r": {Token: "private-test-token"}},
		Contexts:       map[string]*clientapi.Context{"selected": {Cluster: "m", AuthInfo: "r"}},
	}
	data, err := clientcmd.Write(raw)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.request = config.Request{Kubeconfig: filename, Context: "selected", RequestTimeout: time.Second}
	f.expected = generation.ManagementIdentity{APIEndpoint: server.URL + "/prefix", SystemNamespaceUID: "cluster-one", TrustDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(ca))}
	return f
}

func desiredConfigMap() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "selected", "namespace": "dpf-system"}, "data": map[string]any{"setting": "value"}}}
}

func TestNativeClientRechecksIdentityBeforeCreate(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) })
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	f.uid.Store("replacement-cluster")
	_, err = c.Create(context.Background(), desiredConfigMap(), false)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected changed identity rejection, got %v", err)
	}
	if f.writes.Load() != 0 {
		t.Fatal("changed identity caused a write")
	}
}

func TestNativeClientRejectsChangedBindingAndVersion(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	for _, field := range []string{"endpoint", "trust", "version", "uid"} {
		t.Run(field, func(t *testing.T) {
			expected, version := f.expected, "v1.35.6"
			switch field {
			case "endpoint":
				expected.APIEndpoint += "/changed"
			case "trust":
				expected.TrustDigest = "different"
			case "version":
				version = "v1.35.5"
			case "uid":
				expected.SystemNamespaceUID = "other"
			}
			if _, err := NewAPIClient(context.Background(), f.request, expected, version); err == nil {
				t.Fatal("changed binding accepted")
			}
		})
	}
	if f.writes.Load() != 0 {
		t.Fatal("binding check wrote resources")
	}
}

func TestNativeClientCreateDryRunAndInputIsolation(t *testing.T) {
	var dryRun atomic.Bool
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/prefix/api/v1/namespaces/dpf-system/configmaps" {
			http.Error(w, "unexpected", 500)
			return
		}
		dryRun.Store(r.URL.Query().Get("dryRun") == "All")
		var object map[string]any
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		object["metadata"].(map[string]any)["uid"] = "created"
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(object)
	})
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	desired := desiredConfigMap()
	before := desired.DeepCopy()
	created, err := c.Create(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetUID() != "created" || !dryRun.Load() || f.writes.Load() != 1 {
		t.Fatal("dry-run create contract not preserved")
	}
	if !reflect.DeepEqual(desired, before) {
		t.Fatal("desired resource was mutated")
	}
}

func TestNativeClientRedactsServerErrorsWithoutLosingClassification(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","message":"private-test-token secret payload","code":409}`)
	})
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(context.Background(), desiredConfigMap(), false)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("classification lost: %v", err)
	}
	if strings.Contains(err.Error(), "private-test-token") || strings.Contains(err.Error(), "secret payload") {
		t.Fatal("server echoed sensitive text")
	}
}

func TestNativeClientListsAcrossNamespacesButRejectsPartialPages(t *testing.T) {
	var allNamespaces atomic.Bool
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		allNamespaces.Store(r.URL.Path == "/prefix/api/v1/configmaps")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"ConfigMapList","metadata":{"continue":"repeated"},"items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"selected","namespace":"dpf-system"}}]}`)
	})
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	template := desiredConfigMap()
	template.SetNamespace("")
	objects, err := c.List(context.Background(), template, "")
	if err == nil || objects != nil || !allNamespaces.Load() {
		t.Fatalf("partial/all-namespace handling: objects=%v error=%v", objects, err)
	}
	if f.writes.Load() != 0 {
		t.Fatal("listing wrote resources")
	}
}

func TestNativeClientRejectsScopeAndServerStateAndCancellation(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) })
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) { o.SetNamespace("") },
		func(o *unstructured.Unstructured) { o.SetKind("Unreviewed") },
		func(o *unstructured.Unstructured) { o.SetUID("old") },
		func(o *unstructured.Unstructured) { o.Object["status"] = map[string]any{} },
	} {
		desired := desiredConfigMap()
		mutate(desired)
		if _, err := c.Create(context.Background(), desired, false); err == nil {
			t.Fatal("invalid create accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, desiredConfigMap()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation classification lost: %v", err)
	}
	if f.writes.Load() != 0 {
		t.Fatal("invalid/canceled request wrote resources")
	}
}

func TestNativeIdentityCancellationAndEvidence(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) })
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := c.access.ObserveIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(identity.EvidenceRefs, []string{"management-identity"}) || identity.Nodes != nil {
		t.Fatalf("identity-only read claims uncollected nodes: %#v", identity.EvidenceRefs)
	}
	f.stallIdentity.Store(true)
	for _, operation := range []string{"constructor", "create", "get", "list"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var err error
			switch operation {
			case "constructor":
				_, err = NewAPIClient(ctx, f.request, f.expected, "v1.35.6")
			case "create":
				_, err = c.Create(ctx, desiredConfigMap(), false)
			case "get":
				_, err = c.Get(ctx, desiredConfigMap())
			case "list":
				_, err = c.List(ctx, desiredConfigMap(), "")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("identity deadline classification lost: %v", err)
			}
		})
	}
	if f.writes.Load() != 0 {
		t.Fatal("canceled identity check caused a write")
	}
}

func TestNativeReadCannotReuseStaleClusterBinding(t *testing.T) {
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) })
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	f.uid.Store("replacement-cluster")
	if _, err := c.Get(context.Background(), desiredConfigMap()); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("read reused stale cluster: %v", err)
	}
	if objects, err := c.List(context.Background(), desiredConfigMap(), ""); err == nil || objects != nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("list reused stale cluster: %v", err)
	}
}

func TestNativeClientRejectsReplacementDuringReadOrCreate(t *testing.T) {
	for _, operation := range []string{"get", "create", "list"} {
		t.Run(operation, func(t *testing.T) {
			var f *apiFixture
			f = newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				f.uid.Store("replacement-cluster")
				if operation == "list" {
					fmt.Fprint(w, `{"kind":"ConfigMapList","apiVersion":"v1","items":[]}`)
				} else {
					_ = json.NewEncoder(w).Encode(desiredConfigMap().Object)
				}
			})
			c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "get":
				_, err = c.Get(context.Background(), desiredConfigMap())
			case "create":
				_, err = c.Create(context.Background(), desiredConfigMap(), false)
			case "list":
				_, err = c.List(context.Background(), desiredConfigMap(), "")
			}
			if err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("replacement data was accepted: %v", err)
			}
		})
	}
}
