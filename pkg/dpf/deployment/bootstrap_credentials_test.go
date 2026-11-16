// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/cluster-bootstrap/token/jws"
)

const bootstrapTestID = "abcdef"
const bootstrapTestSecret = "0123456789abcdef"

type bootstrapManagementFixture struct {
	dpu, join     *unstructured.Unstructured
	writes, reads int
	beforeRead    func(int)
}

func (f *bootstrapManagementFixture) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.reads++
	if f.beforeRead != nil {
		f.beforeRead(f.reads)
	}
	if desired.GetKind() == "DPU" {
		return f.dpu.DeepCopy(), nil
	}
	if f.join != nil {
		return f.join.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, desired.GetName())
}
func (f *bootstrapManagementFixture) Create(context.Context, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	f.writes++
	return nil, fmt.Errorf("unexpected mutation")
}

type bootstrapTenantFixture struct {
	nodeName              string
	mu                    sync.Mutex
	node                  *corev1.Node
	token                 *corev1.Secret
	info                  *corev1.ConfigMap
	forbidden, mutatePath string
	reads                 map[string]int
	writes                int
}

func (f *bootstrapTenantFixture) serve(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") && !strings.Contains(r.URL.Path, "/secrets/") && !strings.Contains(r.URL.Path, "/configmaps/") {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[r.URL.Path]++
	if r.Method != "GET" {
		f.writes++
		http.Error(w, "unexpected mutation", 500)
		return true
	}
	status := http.StatusNotFound
	var object any
	switch r.URL.Path {
	case "/api/v1/nodes/" + f.nodeName:
		object = f.node
		if f.node != nil {
			status = 200
		}
	case "/api/v1/namespaces/kube-system/secrets/bootstrap-token-" + bootstrapTestID:
		object = f.token
		if f.token != nil {
			status = 200
		}
	case "/api/v1/namespaces/kube-public/configmaps/cluster-info":
		object = f.info
		if f.info != nil {
			status = 200
		}
	}
	if r.URL.Path == f.forbidden {
		status = 403
	}
	if f.mutatePath == r.URL.Path && f.reads[r.URL.Path] == 2 {
		if f.token != nil && strings.Contains(r.URL.Path, "/secrets/") {
			f.token.ResourceVersion = "changed"
		}
		if f.info != nil && strings.Contains(r.URL.Path, "/configmaps/") {
			f.info.ResourceVersion = "changed"
		}
	}
	w.WriteHeader(status)
	if status == 200 {
		_ = json.NewEncoder(w).Encode(object)
	} else {
		_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: map[int]metav1.StatusReason{404: metav1.StatusReasonNotFound, 403: metav1.StatusReasonForbidden}[status], Code: int32(status), Message: "private-error-canary"})
	}
	return true
}
func newBootstrapCredentialFixture(t *testing.T) (*bootstrapManagementFixture, *tenantClient, *bootstrapTenantFixture, *unstructured.Unstructured) {
	t.Helper()
	expected, sources := tenantSourceFixture(t)
	dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU", "metadata": map[string]any{"name": "selected-dpu", "namespace": expected.Namespace, "uid": "selected-dpu-uid", "resourceVersion": "1", "generation": int64(1)}, "spec": map[string]any{"cluster": map[string]any{"name": expected.Name, "namespace": expected.Namespace}}, "status": map[string]any{"phase": "DPU Cluster Config"}}}
	management, state := bootstrapCredentialMaterials(t, dpu, sources.secret.Data["super-admin.conf"])
	tenant, _ := tenantIdentityClientFixtureWithHandler(t, expected, &tenantSourceStub{sources: sources}, state.serve)
	return management, tenant, state, dpu
}

func bootstrapCredentialMaterials(t *testing.T, dpu *unstructured.Unstructured, adminData []byte) (*bootstrapManagementFixture, *bootstrapTenantFixture) {
	t.Helper()
	admin, err := clientcmd.Load(adminData)
	if err != nil {
		t.Fatal(err)
	}
	cluster := admin.Clusters[admin.Contexts[admin.CurrentContext].Cluster]
	block, _ := pem.Decode(cluster.CertificateAuthorityData)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	command := "kubeadm join " + strings.TrimPrefix(cluster.Server, "https://") + " --token " + bootstrapTestID + "." + bootstrapTestSecret + " --v=5 --discovery-token-ca-cert-hash sha256:" + hex.EncodeToString(pin[:])
	join := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: dpu.GetName() + "-kubeadm-join", Namespace: dpu.GetNamespace(), UID: "join-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{{APIVersion: dpu.GetAPIVersion(), Kind: "DPU", Name: dpu.GetName(), UID: dpu.GetUID()}}}, Data: map[string][]byte{"join": []byte(command)}}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(join)
	if err != nil {
		t.Fatal(err)
	}
	management := &bootstrapManagementFixture{dpu: dpu.DeepCopy(), join: &unstructured.Unstructured{Object: object}}
	publicConfig := clientapi.Config{Clusters: map[string]*clientapi.Cluster{"tenant": cluster.DeepCopy()}}
	data, err := clientcmd.Write(publicConfig)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := jws.ComputeDetachedSignature(string(data), bootstrapTestID, bootstrapTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	state := &bootstrapTenantFixture{nodeName: dpu.GetName(), reads: map[string]int{}, token: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + bootstrapTestID, Namespace: "kube-system", UID: "token-uid", ResourceVersion: "1", Labels: map[string]string{"provisioning.dpu.nvidia.com/dpu-name": dpu.GetName(), "provisioning.dpu.nvidia.com/dpu-namespace": dpu.GetNamespace()}}, Type: corev1.SecretTypeBootstrapToken, Data: map[string][]byte{"token-id": []byte(bootstrapTestID), "token-secret": []byte(bootstrapTestSecret), "expiration": []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), "usage-bootstrap-authentication": []byte("true"), "usage-bootstrap-signing": []byte("true"), "auth-extra-groups": []byte("system:bootstrappers:kubeadm:default-node-token")}}, info: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public", UID: "info-uid", ResourceVersion: "1"}, Data: map[string]string{"kubeconfig": string(data), "jws-kubeconfig-" + bootstrapTestID: signature}}}
	return management, state
}

func TestAbsentRegistrationBootstrapCredentials(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*bootstrapManagementFixture, *tenantClient, *bootstrapTenantFixture)
	}{
		{name: "future expiration passes"},
		{name: "missing join", code: "BOOTSTRAP_JOIN_SECRET_UNAVAILABLE", edit: func(m *bootstrapManagementFixture, _ *tenantClient, _ *bootstrapTenantFixture) { m.join = nil }},
		{name: "foreign owner", code: "BOOTSTRAP_JOIN_SECRET_UNVERIFIED", edit: func(m *bootstrapManagementFixture, _ *tenantClient, _ *bootstrapTenantFixture) {
			owners := m.join.GetOwnerReferences()
			owners[0].UID = "foreign"
			m.join.SetOwnerReferences(owners)
		}},
		{name: "shell command", code: "BOOTSTRAP_JOIN_SECRET_UNVERIFIED", edit: func(m *bootstrapManagementFixture, _ *tenantClient, _ *bootstrapTenantFixture) {
			_ = unstructured.SetNestedMap(m.join.Object, map[string]any{"join": "cHJpdmF0ZS1lcnJvci1jYW5hcnk="}, "data")
		}},
		{name: "missing token", code: "BOOTSTRAP_TOKEN_UNAVAILABLE", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) { s.token = nil }},
		{name: "expired", code: "BOOTSTRAP_TOKEN_EXPIRED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.token.Data["expiration"] = []byte(time.Now().Add(-time.Second).UTC().Format(time.RFC3339))
		}},
		{name: "malformed expiration", code: "BOOTSTRAP_TOKEN_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.token.Data["expiration"] = []byte("private-error-canary")
		}},
		{name: "wrong usage", code: "BOOTSTRAP_TOKEN_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			delete(s.token.Data, "usage-bootstrap-signing")
		}},
		{name: "wrong token", code: "BOOTSTRAP_TOKEN_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.token.Data["token-secret"] = []byte("private-error-canary")
		}},
		{name: "foreign cohort", code: "BOOTSTRAP_TOKEN_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.token.Labels["provisioning.dpu.nvidia.com/dpu-name"] = "foreign"
		}},
		{name: "missing signature", code: "BOOTSTRAP_JWS_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			delete(s.info.Data, "jws-kubeconfig-"+bootstrapTestID)
		}},
		{name: "bad signature", code: "BOOTSTRAP_JWS_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.info.Data["jws-kubeconfig-"+bootstrapTestID] = "private-error-canary"
		}},
		{name: "token forbidden", code: "BOOTSTRAP_TOKEN_UNAVAILABLE", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.forbidden = "/api/v1/namespaces/kube-system/secrets/bootstrap-token-" + bootstrapTestID
		}},
		{name: "node forbidden", code: "BOOTSTRAP_REGISTRATION_UNMEASURED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.forbidden = "/api/v1/nodes/selected-dpu"
		}},
		{name: "token changes", code: "BOOTSTRAP_CREDENTIALS_CHANGED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.mutatePath = "/api/v1/namespaces/kube-system/secrets/bootstrap-token-" + bootstrapTestID
		}},
		{name: "signature changes", code: "BOOTSTRAP_CREDENTIALS_CHANGED", edit: func(_ *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.mutatePath = "/api/v1/namespaces/kube-public/configmaps/cluster-info"
		}},
		{name: "DPU replacement", code: "BOOTSTRAP_DPU_CHANGED", edit: func(m *bootstrapManagementFixture, _ *tenantClient, _ *bootstrapTenantFixture) {
			m.dpu.SetUID("foreign")
		}},
		{name: "tenant drift", code: "BOOTSTRAP_TENANT_UNVERIFIED", edit: func(_ *bootstrapManagementFixture, c *tenantClient, _ *bootstrapTenantFixture) {
			c.source.(*tenantSourceStub).sources.observed.DPUCluster.SetUID("foreign")
		}},
		{name: "stale agent success does not waive missing token", code: "BOOTSTRAP_TOKEN_UNAVAILABLE", edit: func(m *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			s.token = nil
			_ = unstructured.SetNestedSlice(m.dpu.Object, []any{map[string]any{"type": "KubeletConfigured", "status": "True"}}, "status", "agentStatus", "conditions")
		}},
		{name: "registered NotReady needs no credentials", edit: func(m *bootstrapManagementFixture, _ *tenantClient, s *bootstrapTenantFixture) {
			m.join = nil
			s.token = nil
			s.info = nil
			s.node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "selected-dpu", UID: "registered-uid", ResourceVersion: "1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			if tc.edit != nil {
				tc.edit(m, c, s)
			}
			before := dpu.DeepCopy()
			err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, nil)
			if tc.code == "" && err != nil || tc.code != "" && (err == nil || !strings.Contains(err.Error(), tc.code)) {
				t.Fatalf("result=%v want=%s", err, tc.code)
			}
			if err != nil && (strings.Contains(err.Error(), bootstrapTestSecret) || strings.Contains(err.Error(), bootstrapTestID) || strings.Contains(err.Error(), "private-error-canary")) {
				t.Fatalf("secret leaked: %v", err)
			}
			if m.writes != 0 || s.writes != 0 || !reflect.DeepEqual(before, dpu) {
				t.Fatal("guard mutated state")
			}
		})
	}
}

func TestBootstrapCredentialCancellation(t *testing.T) {
	m, c, _, dpu := newBootstrapCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyAbsentRegistrationCredentials(ctx, m, c, dpu, nil); err == nil || m.reads != 0 {
		t.Fatalf("canceled inspection: err=%v reads=%d", err, m.reads)
	}
}

func TestBootstrapJoinGrammarAndTenantTrust(t *testing.T) {
	m, c, _, dpu := newBootstrapCredentialFixture(t)
	join, ok := bootstrapJoinSecret(m.join, dpu)
	if !ok {
		t.Fatal("fixture join is invalid")
	}
	sources := c.source.(*tenantSourceStub).sources
	admin, err := clientcmd.Load(sources.secret.Data["super-admin.conf"])
	if err != nil {
		t.Fatal(err)
	}
	cluster := admin.Clusters[admin.Contexts[admin.CurrentContext].Cluster]
	command := string(join.Data["join"])
	for name, edit := range map[string]func(string) string{
		"shell suffix":     func(s string) string { return s + "; private-error-canary" },
		"newline":          func(s string) string { return strings.Replace(s, " --token ", "\n--token ", 1) },
		"empty field":      func(s string) string { return strings.Replace(s, " --token ", "  --token ", 1) },
		"foreign endpoint": func(s string) string { return strings.Replace(s, "192.0.2.20", "192.0.2.21", 1) },
		"unknown option":   func(s string) string { return s + " --ignore-preflight-errors all" },
		"missing CA":       func(s string) string { return strings.Split(s, " --discovery-token-ca-cert-hash")[0] },
		"foreign CA":       func(s string) string { return s[:len(s)-64] + strings.Repeat("0", 64) },
		"duplicate CA":     func(s string) string { return s + " --discovery-token-ca-cert-hash " + strings.Fields(s)[7] },
		"oversize":         func(s string) string { return s + strings.Repeat(" ", 8193) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := parseBootstrapJoin(edit(command), cluster.Server, cluster.CertificateAuthorityData); ok {
				t.Fatal("unsupported grammar accepted")
			}
		})
	}
}

func TestBootstrapSignedPublicInfoStillRequiresBoundTenantTrust(t *testing.T) {
	for _, part := range []string{"endpoint", "CA"} {
		t.Run(part, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			config, err := clientcmd.Load([]byte(s.info.Data["kubeconfig"]))
			if err != nil {
				t.Fatal(err)
			}
			for _, cluster := range config.Clusters {
				if part == "endpoint" {
					cluster.Server = "https://192.0.2.99:30538"
				} else {
					cluster.CertificateAuthorityData = []byte("private-error-canary")
				}
			}
			data, err := clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			signature, err := jws.ComputeDetachedSignature(string(data), bootstrapTestID, bootstrapTestSecret)
			if err != nil {
				t.Fatal(err)
			}
			s.info.Data["kubeconfig"] = string(data)
			s.info.Data["jws-kubeconfig-"+bootstrapTestID] = signature
			if err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, nil); err == nil || !strings.Contains(err.Error(), "BOOTSTRAP_JWS_UNVERIFIED") {
				t.Fatalf("signed foreign trust accepted: %v", err)
			}
		})
	}
}

func TestBootstrapCredentialFailureJournalErrorsRemainSafe(t *testing.T) {
	m, c, s, dpu := newBootstrapCredentialFixture(t)
	s.token = nil
	journal := &executionJournal{file: &failingJournalFile{writeErr: fmt.Errorf("private-error-canary")}, digest: "sha256:fixture"}
	err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, journal)
	if err == nil || !strings.Contains(err.Error(), "BOOTSTRAP_TOKEN_UNAVAILABLE") || !strings.Contains(err.Error(), "could not persist") || strings.Contains(err.Error(), "private-error-canary") || m.writes != 0 || s.writes != 0 {
		t.Fatalf("journal failure not safe: %v", err)
	}
	outer := activationFailure(nil, "EXTERNAL_HOST_REBOOT_UNCERTAIN", "Completion unknown.", err)
	if !strings.Contains(outer.Error(), "BOOTSTRAP_TOKEN_UNAVAILABLE") || strings.Contains(outer.Error(), "private-error-canary") {
		t.Fatalf("safe submission cause lost: %v", outer)
	}
	if diagnostic := bootstrapCredentialDiagnostic(fmt.Errorf("private-error-canary")); diagnostic != "" {
		t.Fatalf("unknown cause echoed: %s", diagnostic)
	}
}
