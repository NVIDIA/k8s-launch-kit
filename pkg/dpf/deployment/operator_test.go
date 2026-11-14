// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestEnsureOperatorInstallsExactPinnedChart(t *testing.T) {
	request := operatorTestRequest(t)
	archiveBefore := bytes.Clone(request.Archive)
	valuesBefore := map[string]any{"controllerManager": map[string]any{"replicas": 2}}
	request.Inputs.Values = valuesBefore
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() with values error = %v", err)
	}
	actions := &fakeOperatorActions{getErr: driver.ErrReleaseNotFound, installRelease: releaseForPlan(t, plan)}
	if err := ensureOperator(context.Background(), plan, actions); err != nil {
		t.Fatalf("ensureOperator() error = %v", err)
	}
	if len(actions.installs) != 1 {
		t.Fatalf("install calls = %d, want 1", len(actions.installs))
	}
	installed := actions.installs[0]
	if installed.releaseName != request.Inputs.ReleaseName || installed.namespace != request.Inputs.Bootstrap.OperatorNamespace {
		t.Fatalf("install identity = %s/%s", installed.namespace, installed.releaseName)
	}
	if !installed.wait || installed.timeout != request.Timeout {
		t.Fatalf("install wait/timeout = %t/%s, want true/%s", installed.wait, installed.timeout, request.Timeout)
	}
	if installed.postRenderer == nil {
		t.Fatal("install post-renderer is nil")
	}
	if got, err := chartFingerprint(installed.chart); err != nil || got != plan.chartFingerprint {
		t.Fatalf("install chart fingerprint = %q, %v; want %q", got, err, plan.chartFingerprint)
	}
	if got, err := canonicalJSON(installed.values); err != nil || !bytes.Equal(got, plan.expectedValuesJSON) {
		t.Fatalf("install values = %s, %v; want %s", got, err, plan.expectedValuesJSON)
	}
	postRendered, err := installed.postRenderer.Run(bytes.NewBuffer(plan.renderedManifest))
	if err != nil {
		t.Fatalf("install post-renderer rejected the retained projection: %v", err)
	}
	if !strings.Contains(postRendered.String(), "--config-namespace=custom-dpf-operator-system") || !strings.Contains(postRendered.String(), "--config-name=custom-dpfoperatorconfig") {
		t.Fatalf("install post-renderer did not retain selected DPFOperatorConfig binding: %s", postRendered.String())
	}
	if !bytes.Equal(request.Archive, archiveBefore) {
		t.Fatal("prepareOperator or ensureOperator mutated the retained chart archive")
	}
	if request.Inputs.Values["controllerManager"].(map[string]any)["replicas"] != 2 {
		t.Fatal("prepareOperator or ensureOperator mutated Helm values")
	}
}

func TestEnsureOperatorAcceptsOnlyAnExactDeployedRelease(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	actions := &fakeOperatorActions{installed: releaseForPlan(t, plan)}
	if err := ensureOperator(context.Background(), plan, actions); err != nil {
		t.Fatalf("ensureOperator() exact release error = %v", err)
	}
	if len(actions.installs) != 0 {
		t.Fatalf("exact release caused %d installs, want none", len(actions.installs))
	}
}

func TestEnsureOperatorRejectsInstalledReleaseDriftWithoutUpgrade(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*release.Release)
	}{
		{name: "wrong name", mutate: func(installed *release.Release) { installed.Name = "foreign-release" }},
		{name: "wrong namespace", mutate: func(installed *release.Release) { installed.Namespace = "foreign-namespace" }},
		{name: "pending status", mutate: func(installed *release.Release) { installed.Info.Status = release.StatusPendingInstall }},
		{name: "different chart", mutate: func(installed *release.Release) { installed.Chart.Values["extra"] = true }},
		{name: "different values", mutate: func(installed *release.Release) { installed.Config["extra"] = true }},
		{name: "different rendered manifest", mutate: func(installed *release.Release) {
			installed.Manifest += "\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: dpf-operator-system\n"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installed := releaseForPlan(t, plan)
			test.mutate(installed)
			actions := &fakeOperatorActions{installed: installed}
			if err := ensureOperator(context.Background(), plan, actions); err == nil {
				t.Fatal("ensureOperator() accepted a conflicting Helm release")
			}
			if len(actions.installs) != 0 {
				t.Fatalf("conflicting release caused %d installs, want none", len(actions.installs))
			}
		})
	}
}

func TestEnsureOperatorDoesNotInstallAfterReleaseInspectionFailure(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	inspectionErr := errors.New("permission denied")
	actions := &fakeOperatorActions{getErr: inspectionErr}
	err = ensureOperator(context.Background(), plan, actions)
	if err == nil {
		t.Fatal("ensureOperator() returned nil for an inspection failure")
	}
	if strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ensureOperator() exposed raw API error text: %v", err)
	}
	if len(actions.installs) != 0 {
		t.Fatalf("inspection error caused %d installs, want none", len(actions.installs))
	}
}

func TestEnsureOperatorRejectsRenderedResourceCollisionBeforeInstall(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	guardErr := errors.New("a rendered DPF chart resource already exists")
	actions := &fakeOperatorActions{getErr: driver.ErrReleaseNotFound, resourceCheckErr: guardErr}
	err = ensureOperator(context.Background(), plan, actions)
	if err == nil || !strings.Contains(err.Error(), "not safe to install") {
		t.Fatalf("ensureOperator() error = %v, want fail-closed resource collision", err)
	}
	if len(actions.installs) != 0 {
		t.Fatalf("collision caused %d Helm installs, want none", len(actions.installs))
	}
	if actions.resourceChecks != 1 {
		t.Fatalf("preflight checks = %d, want 1", actions.resourceChecks)
	}
}

func TestEnsureOperatorDoesNotExposeResourceGuardErrors(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	actions := &fakeOperatorActions{getErr: driver.ErrReleaseNotFound, resourceCheckErr: errors.New("secret request path and server detail")}
	err = ensureOperator(context.Background(), plan, actions)
	if err == nil || strings.Contains(err.Error(), "secret request path") {
		t.Fatalf("ensureOperator() error = %v, want a redacted guard failure", err)
	}
	if len(actions.installs) != 0 {
		t.Fatalf("guard error caused %d Helm installs, want none", len(actions.installs))
	}
}

func TestCreateOnlyKubeClientRejectsHelmAdoption(t *testing.T) {
	delegate := &updateSpyKubeClient{}
	client := createOnlyKubeClient{Interface: delegate}
	if _, err := client.Update(nil, nil, false); err == nil || !strings.Contains(err.Error(), "cannot update or adopt") {
		t.Fatalf("Update() error = %v, want adoption rejection", err)
	}
	if delegate.updates != 0 {
		t.Fatalf("adoption guard delegated %d updates", delegate.updates)
	}
	if _, ok := any(client).(kube.InterfaceThreeWayMerge); ok {
		t.Fatal("create-only wrapper unexpectedly exposes Helm's three-way adoption interface")
	}
}

func TestHelmActionClientResourceGuardChecksExactRenderedIdentities(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    string
	}{
		{name: "all absent", statusCode: http.StatusNotFound},
		{name: "existing collision", statusCode: http.StatusOK, wantErr: "already exists"},
		{name: "permission denied", statusCode: http.StatusForbidden, wantErr: "cannot verify absence"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/namespaces/custom-dpf-operator-system" {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				if request.URL.Path != "/api/v1/namespaces/custom-dpf-operator-system/configmaps/operator" {
					t.Errorf("GET path = %s", request.URL.Path)
				}
				if test.statusCode == http.StatusOK {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(test.statusCode)
					_, _ = io.WriteString(writer, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"operator","namespace":"custom-dpf-operator-system","labels":{"app.kubernetes.io/managed-by":"Helm"},"annotations":{"meta.helm.sh/release-name":"dpf","meta.helm.sh/release-namespace":"custom-dpf-operator-system"}}}`)
					return
				}
				writer.WriteHeader(test.statusCode)
			}))
			defer server.Close()
			client := resourceRESTClient(t, server.URL)
			resourceInfo := &resource.Info{
				Client:    client,
				Mapping:   &meta.RESTMapping{Resource: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Scope: meta.RESTScopeNamespace},
				Namespace: "custom-dpf-operator-system",
				Name:      "operator",
			}
			actions := &fixedBuildKubeClient{resources: kube.ResourceList{resourceInfo}}
			actionConfig := &action.Configuration{KubeClient: actions}
			err := (helmActionClient{config: actionConfig}).CheckNoRenderedResources(context.Background(), []byte("manifest"))
			if test.name == "existing collision" {
				if getErr := resourceInfo.Get(); getErr != nil {
					t.Fatalf("direct Info.Get() error = %v", getErr)
				}
			}
			if test.wantErr == "" && err != nil {
				t.Fatalf("CheckNoRenderedResources() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("CheckNoRenderedResources() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestHelmActionClientResourceGuardRejectsNamespaceBeforeRead(t *testing.T) {
	resourceInfo := &resource.Info{Mapping: &meta.RESTMapping{
		Resource:         schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
		GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"},
		Scope:            meta.RESTScopeRoot,
	}}
	client := &fixedBuildKubeClient{resources: kube.ResourceList{resourceInfo}}
	guard := helmActionClient{config: &action.Configuration{KubeClient: client}}
	if err := guard.CheckNoRenderedResources(context.Background(), []byte("manifest")); err == nil || !strings.Contains(err.Error(), "must not manage namespaces") {
		t.Fatalf("CheckNoRenderedResources() error = %v, want namespace projection rejection", err)
	}
}

func TestValidateInstallableChartShapeRejectsAdditionalEffectPaths(t *testing.T) {
	for _, operatorChart := range []*chart.Chart{
		{Files: []*chart.File{{Name: "crds/example.yaml", Data: []byte("apiVersion: apiextensions.k8s.io/v1")}}},
		{Templates: []*chart.File{{Name: "templates/hook.yaml", Data: []byte("metadata:\n  annotations:\n    helm.sh/hook: pre-install")}}},
	} {
		if err := validateInstallableChartShape(operatorChart); err == nil {
			t.Fatal("validateInstallableChartShape() accepted unsupported effects")
		}
	}
}

func TestCanonicalManifestRejectsNamespaceResources(t *testing.T) {
	manifest := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: dpf-system\n")
	if _, err := canonicalManifest(manifest); err == nil || !strings.Contains(err.Error(), "must not manage namespaces") {
		t.Fatalf("canonicalManifest() error = %v, want namespace rejection", err)
	}
}

func resourceRESTClient(t *testing.T, serverURL string) *rest.RESTClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core API types: %v", err)
	}
	config := &rest.Config{
		Host: serverURL, APIPath: "/api",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{Version: "v1"},
			NegotiatedSerializer: serializer.NewCodecFactory(scheme).WithoutConversion(),
		},
	}
	client, err := rest.RESTClientFor(config)
	if err != nil {
		t.Fatalf("create REST client: %v", err)
	}
	return client
}

func TestEnsureOperatorCancellationStopsFurtherActionCalls(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	actions := &fakeOperatorActions{getWait: true, getStarted: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ensureOperator(ctx, plan, actions) }()
	<-actions.getStarted
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ensureOperator() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ensureOperator() did not stop after context cancellation")
	}
	if len(actions.installs) != 0 {
		t.Fatalf("cancellation caused %d installs, want none", len(actions.installs))
	}
}

func TestEnsureOperatorReturnsCancellationAfterInstallWithoutRollback(t *testing.T) {
	request := operatorTestRequest(t)
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatalf("prepareOperator() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	actions := &fakeOperatorActions{getErr: driver.ErrReleaseNotFound, installRelease: releaseForPlan(t, plan), cancelOnInstall: cancel}
	err = ensureOperator(ctx, plan, actions)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ensureOperator() error = %v, want context cancellation", err)
	}
	if len(actions.installs) != 1 {
		t.Fatalf("install calls = %d, want one completed install", len(actions.installs))
	}
}

func TestContextRoundTripperHonorsCallerAndRequestCancellation(t *testing.T) {
	tests := []struct {
		name          string
		cancelRequest bool
	}{
		{name: "caller context"},
		{name: "request context", cancelRequest: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			transport := waitingRoundTripper{started: make(chan struct{})}
			bound := contextRoundTripper{ctx: caller, transport: transport}
			request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://cluster.example/api", nil)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := bound.RoundTrip(request)
				result <- err
			}()
			<-transport.started
			if test.cancelRequest {
				cancelRequest()
			} else {
				cancelCaller()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("RoundTrip() error = %v, want context cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("RoundTrip() did not stop after context cancellation")
			}
		})
	}
}

func TestContextRoundTripperKeepsContextUntilResponseBodyCompletes(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://cluster.example/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &waitingResponseBody{started: make(chan struct{})}
	bound := contextRoundTripper{
		ctx: caller,
		transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			body.ctx = request.Context()
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: request}, nil
		}),
	}
	response, err := bound.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	select {
	case <-body.ctx.Done():
		t.Fatal("request context was canceled when RoundTrip returned headers")
	default:
	}

	readResult := make(chan error, 1)
	go func() {
		_, err := response.Body.Read(make([]byte, 1))
		readResult <- err
	}()
	<-body.started
	cancelCaller()
	select {
	case err := <-readResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("response body read error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response body read did not stop after caller cancellation")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("response body Close() error = %v", err)
	}
}

func TestContextRoundTripperReleasesContextOnBodyEOFAndClose(t *testing.T) {
	for _, test := range []struct {
		name string
		read bool
	}{{name: "EOF", read: true}, {name: "early close"}} {
		t.Run(test.name, func(t *testing.T) {
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://cluster.example/api", nil)
			if err != nil {
				t.Fatal(err)
			}
			body := &trackingResponseBody{Reader: bytes.NewReader([]byte("reply")), closed: make(chan struct{})}
			bound := contextRoundTripper{
				ctx: caller,
				transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					body.ctx = request.Context()
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: request}, nil
				}),
			}
			response, err := bound.RoundTrip(request)
			if err != nil {
				t.Fatalf("RoundTrip() error = %v", err)
			}
			if test.read {
				if data, err := io.ReadAll(response.Body); err != nil || string(data) != "reply" {
					t.Fatalf("ReadAll(body) = %q, %v", data, err)
				}
			} else if err := response.Body.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			select {
			case <-body.closed:
			case <-time.After(time.Second):
				t.Fatal("underlying response body was not closed")
			}
			select {
			case <-body.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("request context remained active after response body cleanup")
			}
		})
	}
}

func TestOperatorRESTConfigCopiesAndBoundsRequests(t *testing.T) {
	original := &rest.Config{Host: "https://cluster.example", Timeout: 20 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cloned, err := operatorRESTConfig(ctx, original, 3*time.Second)
	if err != nil {
		t.Fatalf("operatorRESTConfig() error = %v", err)
	}
	if cloned == original || cloned.Host != original.Host || cloned.Timeout != 3*time.Second {
		t.Fatalf("cloned REST config = %#v; original=%#v", cloned, original)
	}
	if original.Timeout != 20*time.Second || original.WrapTransport != nil {
		t.Fatal("operatorRESTConfig mutated its caller's REST config")
	}
}

func TestEnsureOperatorRejectsInvalidRequestBeforeHelmInitialization(t *testing.T) {
	request := operatorTestRequest(t)
	if err := EnsureOperator(context.Background(), request); err == nil || !strings.Contains(err.Error(), "management REST config is required") {
		t.Fatalf("EnsureOperator() with nil REST config error = %v", err)
	}
	request.RESTConfig = &rest.Config{Host: "https://cluster.example"}
	request.Timeout = 0
	if err := EnsureOperator(context.Background(), request); err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("EnsureOperator() with zero timeout error = %v", err)
	}
	request.Timeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := EnsureOperator(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureOperator() with canceled context error = %v", err)
	}
}

func operatorTestRequest(t *testing.T) OperatorRequest {
	t.Helper()
	archive, err := os.ReadFile("../bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
	if err != nil {
		t.Fatalf("read pinned DPF operator chart fixture: %v", err)
	}
	values := map[string]any{"controllerManager": map[string]any{"replicas": 2}}
	return OperatorRequest{
		Archive: archive,
		Inputs: bootstrap.OperatorRenderInputs{
			ReleaseVersion:    "26.8.0",
			ReleaseName:       "dpf",
			KubernetesVersion: "1.35.4",
			Values:            values,
			Bootstrap: bootstrap.Inputs{
				OperatorNamespace:  "custom-dpf-operator-system",
				OperatorConfigName: "custom-dpfoperatorconfig",
			},
		},
		Timeout: 15 * time.Second,
	}
}

func releaseForPlan(t *testing.T, plan *operatorPlan) *release.Release {
	t.Helper()
	operatorChart, err := loader.LoadArchive(bytes.NewReader(plan.archive))
	if err != nil {
		t.Fatalf("load chart for existing release: %v", err)
	}
	values, err := jsonMap(plan.valuesYAML)
	if err != nil {
		t.Fatalf("decode plan Helm values: %v", err)
	}
	return &release.Release{
		Name:      plan.releaseName,
		Namespace: plan.namespace,
		Chart:     operatorChart,
		Config:    values,
		Manifest:  string(plan.renderedManifest),
		Info:      &release.Info{Status: release.StatusDeployed},
	}
}

func jsonMap(data []byte) (map[string]interface{}, error) {
	var values map[string]interface{}
	if err := sigsyaml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

type fakeOperatorActions struct {
	installed        *release.Release
	getErr           error
	resourceCheckErr error
	resourceChecks   int
	getWait          bool
	getStarted       chan struct{}
	installRelease   *release.Release
	installErr       error
	cancelOnInstall  context.CancelFunc
	installs         []operatorInstall
}

type fixedBuildKubeClient struct {
	kube.Interface
	resources kube.ResourceList
}

type updateSpyKubeClient struct {
	kube.Interface
	updates int
}

func (c *updateSpyKubeClient) Update(kube.ResourceList, kube.ResourceList, bool) (*kube.Result, error) {
	c.updates++
	return nil, nil
}

func (c *fixedBuildKubeClient) Build(io.Reader, bool) (kube.ResourceList, error) {
	return c.resources, nil
}

func (f *fakeOperatorActions) CheckNoRenderedResources(context.Context, []byte) error {
	f.resourceChecks++
	return f.resourceCheckErr
}

func (f *fakeOperatorActions) Get(ctx context.Context, _ string) (*release.Release, error) {
	if f.getWait {
		if f.getStarted != nil {
			close(f.getStarted)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.installed, nil
}

func (f *fakeOperatorActions) Install(_ context.Context, request operatorInstall) (*release.Release, error) {
	f.installs = append(f.installs, request)
	if f.cancelOnInstall != nil {
		f.cancelOnInstall()
	}
	if f.installErr != nil {
		return nil, f.installErr
	}
	return f.installRelease, nil
}

type waitingRoundTripper struct {
	started chan struct{}
}

func (t waitingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	close(t.started)
	<-request.Context().Done()
	return nil, request.Context().Err()
}

var _ http.RoundTripper = waitingRoundTripper{}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type waitingResponseBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *waitingResponseBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *waitingResponseBody) Close() error { return nil }

type trackingResponseBody struct {
	*bytes.Reader
	ctx    context.Context
	closed chan struct{}
}

func (b *trackingResponseBody) Close() error {
	close(b.closed)
	return nil
}
