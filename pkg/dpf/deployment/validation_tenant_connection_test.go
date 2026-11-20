// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

func tenantConnectionTransition(sources tenantSources, scenario string) tenantSources {
	sources.observed.TenantControlPlane = sources.observed.TenantControlPlane.DeepCopy()
	switch scenario {
	case "pending", "identity":
		_ = unstructured.SetNestedField(sources.observed.TenantControlPlane.Object, "NotReady", "status", "kubernetesResources", "version", "status")
		if scenario == "identity" {
			sources.observed.DPUCluster = sources.observed.DPUCluster.DeepCopy()
			sources.observed.DPUCluster.SetUID("replacement")
		}
	case "conflict":
		_ = unstructured.SetNestedField(sources.observed.TenantControlPlane.Object, "192.0.2.99", "spec", "networkProfile", "address")
	case "invalid":
		_ = unstructured.SetNestedField(sources.observed.TenantControlPlane.Object, int64(7), "status", "kubernetesResources", "version", "status")
	}
	return sources
}

// Do not replace the opener: the third observation occurs inside the actual
// production tenant constructor, after both of the adapter's source checks.
func TestValidationTenantNativeThirdSourceClassification(t *testing.T) {
	o, _, reads := newTenantPendingObserver(t)
	for _, test := range []struct {
		name   string
		state  validation.State
		reason validation.Reason
	}{
		{name: "pending", state: validation.Pending, reason: validation.ReasonProgressing},
		{name: "conflict", state: validation.Failed, reason: validation.ReasonHealth},
		{name: "invalid", state: validation.Unverified, reason: validation.ReasonIncomplete},
		{name: "identity", state: validation.Unverified, reason: validation.ReasonIdentityDrift},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			o.management.sources = func() (tenantSources, error) {
				calls++
				if calls == 3 {
					return tenantConnectionTransition(o.fixture.sources, test.name), nil
				}
				return o.fixture.sources, nil
			}
			r := newValidationRecorder(o.checks, nil)
			tenant, err := o.fixture.adapter.native.observeTenantResources(context.Background(), o.management, r)
			state, reason, _, _ := validationErrorFields(err)
			if tenant != nil || err == nil || calls != 3 || state != test.state || reason != test.reason {
				t.Fatalf("constructor source #3: tenant=%v calls=%d state=%s reason=%s err=%v", tenant != nil, calls, state, reason, err)
			}
			if got := r.outcomes["tenant/sources"]; got.State != validation.Passed {
				t.Fatalf("first source observation changed: %+v", got)
			}
			if got := r.outcomes["tenant/api"]; got.State != test.state || got.Reason != test.reason {
				t.Fatalf("native opener lost source classification: %+v", got)
			}
			if test.name == "identity" && !validationIdentityChanged(err) {
				t.Fatal("source progress masked measured replacement")
			}
			for path, count := range reads {
				if count != 0 {
					t.Fatalf("source rejection attempted networking read: %s %d", path, count)
				}
			}
		})
	}
}

type tenantConnectionRunnerObserver struct {
	fixture             *tenantPendingObserver
	calls               int
	sourceCalls         int
	first               validation.ResourceSnapshot
	healthyAfterPending bool
	open                func(context.Context, validationManagementReader) (*tenantClient, error)
}

func (o *tenantConnectionRunnerObserver) ObserveResources(ctx context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	o.calls++
	active := []validation.CheckSpec{}
	for _, spec := range o.fixture.checks {
		if !spec.FullOnly {
			active = append(active, spec)
		}
	}
	r := newValidationRecorder(active, progress)
	for _, spec := range o.fixture.independent {
		if !spec.FullOnly {
			r.record(spec.ID, validation.Passed, validation.ReasonNone, "Independent fixture boundary.", "")
		}
	}
	o.fixture.management.sources = func() (tenantSources, error) {
		o.sourceCalls++
		if o.sourceCalls == 3 {
			return tenantConnectionTransition(o.fixture.fixture.sources, "pending"), nil
		}
		if o.calls > 1 && !o.healthyAfterPending {
			return tenantConnectionTransition(o.fixture.fixture.sources, "conflict"), nil
		}
		return o.fixture.fixture.sources, nil
	}
	native := o.fixture.fixture.adapter.native
	open := o.open
	if open == nil {
		open = native.openValidationTenant
	}
	tenant, err := native.observeTenantResourcesWithOpener(ctx, o.fixture.management, r, open)
	if validationIdentityChanged(err) {
		return r.snapshot(), err
	}
	if err := native.observeValidationTenantDNS(ctx, o.fixture.management, tenant, o.fixture.hostDNS, r); err != nil {
		return r.snapshot(), err
	}
	if err := native.observeValidationKubeProxy(ctx, o.fixture.management, tenant, r); err != nil {
		return r.snapshot(), err
	}
	if err := native.observeValidationRuntimeResourcesWithImage(ctx, o.fixture.management, tenant, r, func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
		return o.fixture.fixture.adapter.image, nil
	}); err != nil {
		return r.snapshot(), err
	}
	r.blockRemaining()
	if o.calls == 1 {
		o.first = r.snapshot()
	}
	return r.snapshot(), nil
}
func (*tenantConnectionRunnerObserver) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	return nil, errors.New("resource-only regression cannot open runtime")
}

func TestValidationTenantThirdSourcePendingLetsNativeRunnerPollAgain(t *testing.T) {
	fixture, plan, _ := newTenantPendingObserver(t)
	o := &tenantConnectionRunnerObserver{fixture: fixture}
	runner, err := validation.NewRunner(plan, o, validation.Options{Wait: 30 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if o.calls != 2 || o.sourceCalls != 4 || result.Verdict != validation.VerdictFailed {
		t.Fatalf("native constructor Pending did not allow the next classified snapshot: polls=%d sourceCalls=%d verdict=%s first=%+v", o.calls, o.sourceCalls, result.Verdict, o.first)
	}
	for _, row := range o.first.Checks {
		if row.State != validation.Passed && row.State != validation.Pending {
			t.Fatalf("constructor source progress produced a blocker: %+v", row)
		}
	}
	// The second snapshot deliberately reports confirmed conflicting source
	// intent. Polling must stop with that measured failure, never acceptance.
	for _, row := range result.Checks {
		if row.ID == "tenant/sources" && row.Outcome.State != validation.Failed {
			t.Fatalf("later conflict was hidden: %+v", row)
		}
	}
}

type tenantConnectionSource struct {
	read func(context.Context) (tenantSources, error)
}

func (s tenantConnectionSource) observeTenantSources(ctx context.Context, _ controllercontract.TenantReadinessExpected) (tenantSources, error) {
	return s.read(ctx)
}

// Exercise actual credential/TLS construction and the constructor's final
// source fence. Only the network dial maps the retained VIP to the local server.
func tenantConnectionTLSFixture(t *testing.T) (controllercontract.TenantReadinessExpected, tenantSources, tenantDialContext, *int) {
	t.Helper()
	expected, sources := tenantSourceFixture(t)
	reads := 0
	server, ca := nativeTenantTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("tenant constructor effect: %s", r.Method)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": expected.KubernetesVersion})
		default:
			t.Errorf("unexpected constructor read: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	cfg, err := clientcmd.Load(sources.secret.Data["super-admin.conf"])
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster].CertificateAuthorityData = ca
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	sources.secret = sources.secret.DeepCopy()
	sources.secret.Data["super-admin.conf"] = data
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "192.0.2.20:30538" {
			return nil, fmt.Errorf("unexpected tenant dial %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return expected, sources, dial, &reads
}

func TestValidationTenantConstructorFinalFenceClassification(t *testing.T) {
	expected, sources, dial, reads := tenantConnectionTLSFixture(t)
	for _, test := range []struct {
		name   string
		state  validation.State
		reason validation.Reason
	}{
		{name: "pending", state: validation.Pending, reason: validation.ReasonProgressing},
		{name: "conflict", state: validation.Failed, reason: validation.ReasonHealth},
		{name: "invalid", state: validation.Unverified, reason: validation.ReasonIncomplete},
		{name: "identity", state: validation.Unverified, reason: validation.ReasonIdentityDrift},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, before := 0, *reads
			source := tenantConnectionSource{read: func(context.Context) (tenantSources, error) {
				calls++
				if calls == 2 {
					return tenantConnectionTransition(sources, test.name), nil
				}
				return sources, nil
			}}
			tenant, cause := newTenantClient(context.Background(), testRules(t), source, expected, "dc-uid", time.Second, dial, flowcontrol.NewFakeAlwaysRateLimiter())
			err := validationTenantConnectionError(context.Background(), cause)
			state, reason, _, _ := validationErrorFields(err)
			if tenant != nil || cause == nil || calls != 2 || *reads-before != 2 || state != test.state || reason != test.reason {
				t.Fatalf("constructor final fence: tenant=%v calls=%d APIreads=%d state=%s reason=%s err=%v", tenant != nil, calls, *reads-before, state, reason, err)
			}
			if test.name == "identity" && !validationIdentityChanged(err) {
				t.Fatal("final fence source progress masked replacement")
			}
		})
	}
}

func TestValidationTenantBoundSessionProgressStillBlocks(t *testing.T) {
	expected, sources, dial, reads := tenantConnectionTLSFixture(t)
	calls := 0
	source := tenantConnectionSource{read: func(context.Context) (tenantSources, error) {
		calls++
		if calls >= 3 {
			return tenantConnectionTransition(sources, "pending"), nil
		}
		return sources, nil
	}}
	tenant, err := newTenantClient(context.Background(), testRules(t), source, expected, "dc-uid", time.Second, dial, flowcontrol.NewFakeAlwaysRateLimiter())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || *reads != 2 {
		t.Fatalf("constructor did not close sources: source=%d API=%d", calls, *reads)
	}
	err = tenant.VerifyIdentity(context.Background())
	state, reason, _, _ := validationErrorFields(err)
	if err == nil || calls != 3 || *reads != 2 || state != validation.Unverified || reason != validation.ReasonUnavailable {
		t.Fatalf("established session became retryable or read past lost fence: calls=%d API=%d state=%s reason=%s err=%v", calls, *reads, state, reason, err)
	}
	if !strings.HasPrefix(err.Error(), "tenant source resources are not ready: ") {
		t.Fatalf("legacy private error text changed: %s", err)
	}
}

func TestValidationTenantConnectionProjectionPreservesMixedCauses(t *testing.T) {
	source := &tenantSourceReadinessError{readiness: controllercontract.TenantReadiness{State: controllercontract.TenantReadinessPending, ReasonCode: "TENANT_CONTROL_PLANE_PROGRESSING"}}
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "tenantcontrolplanes"}, "selected", nil)
	for _, test := range []struct {
		name   string
		cause  error
		reason validation.Reason
	}{
		{name: "identity", cause: errTenantIdentityChanged, reason: validation.ReasonIdentityDrift},
		{name: "access", cause: denied, reason: validation.ReasonDenied},
		{name: "canceled", cause: context.Canceled, reason: validation.ReasonCanceled},
		{name: "deadline", cause: context.DeadlineExceeded, reason: validation.ReasonTimedOut},
		{name: "unavailable", cause: errors.New("unavailable"), reason: validation.ReasonUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, incoming := range []error{errors.Join(source, test.cause), errors.Join(test.cause, source)} {
				err := validationTenantConnectionError(context.Background(), incoming)
				state, reason, _, _ := validationErrorFields(err)
				if err != incoming || !errors.Is(err, test.cause) || state != validation.Unverified || reason != test.reason {
					t.Fatalf("constructor projection hid mixed cause: state=%s reason=%s err=%v", state, reason, err)
				}
			}
		})
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		} else {
			cancel()
		}
		err := validationTenantConnectionError(ctx, source)
		cancel()
		state, reason, _, _ := validationErrorFields(err)
		want := validation.ReasonCanceled
		if deadline {
			want = validation.ReasonTimedOut
		}
		if state != validation.Unverified || reason != want || !errors.Is(err, ctx.Err()) {
			t.Fatalf("source Pending hid completed context: %s %s %v", state, reason, err)
		}
	}
	wrapped := fmt.Errorf("source wrapper: %w", source)
	if got := validationTenantConnectionError(context.Background(), wrapped); got != wrapped {
		t.Fatal("projection widened beyond direct constructor result")
	}
}

// A Pending final constructor fence discards the tentative client. A fresh
// constructor on the next Runner snapshot must bind healthy current sources,
// perform real TLS/identity reads, and qualify all affected native resources.
func TestValidationTenantConstructorPendingConvergesThroughRunner(t *testing.T) {
	fixture, plan, _ := newTenantPendingObserver(t)
	client := fixture.fixture.adapter.tenant.api.Discovery().RESTClient().(*rest.RESTClient).Client
	origin := fixture.fixture.adapter.tenant.api.CoreV1().RESTClient().Get().URL()
	server, ca := nativeTenantTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("constructor regression effect: %s", r.Method)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.URL.Scheme, request.URL.Host = origin.Scheme, origin.Host
		response, err := client.Do(request)
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture backend", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			t.Error(err)
		}
	}))
	cfg, err := clientcmd.Load(fixture.fixture.sources.secret.Data["super-admin.conf"])
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster].CertificateAuthorityData = ca
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fixture.sources.secret = fixture.fixture.sources.secret.DeepCopy()
	fixture.fixture.sources.secret.Data["super-admin.conf"] = data
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "192.0.2.20:30538" {
			return nil, fmt.Errorf("unexpected tenant dial %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	o := &tenantConnectionRunnerObserver{fixture: fixture, healthyAfterPending: true}
	openCalls := 0
	o.open = func(ctx context.Context, management validationManagementReader) (*tenantClient, error) {
		openCalls++
		expected, _, err := tenantPlanInputs(fixture.fixture.adapter.native.desired.plan)
		if err != nil {
			return nil, err
		}
		tenant, err := newTenantClient(ctx, fixture.fixture.adapter.native.desired.rules, management, expected, string(fixture.fixture.sources.observed.DPUCluster.GetUID()), time.Second, dial, flowcontrol.NewFakeAlwaysRateLimiter())
		if err != nil {
			return nil, validationTenantConnectionError(ctx, err)
		}
		return tenant, nil
	}
	runner, err := validation.NewRunner(plan, o, validation.Options{Wait: 30 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if o.calls != 2 || openCalls != 2 || result.Verdict != validation.VerdictResourcesPassed {
		for _, row := range result.Checks {
			if row.Outcome.State != validation.Passed && row.Outcome.State != validation.Skipped {
				t.Logf("blocker: %+v", row)
			}
		}
		t.Fatalf("constructor recovery failed: polls=%d connections=%d verdict=%s first=%+v", o.calls, openCalls, result.Verdict, o.first)
	}
	for _, row := range o.first.Checks {
		if row.State != validation.Passed && row.State != validation.Pending {
			t.Fatalf("Pending constructor produced a blocker: %+v", row)
		}
	}
}
