// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/postrender"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	sigsyaml "sigs.k8s.io/yaml"
)

// OperatorRequest supplies the selected DPF chart bytes and explicit
// management-cluster connection. It does not install or modify any DPF CR.
type OperatorRequest struct {
	RESTConfig *rest.Config
	Archive    []byte
	Inputs     bootstrap.OperatorRenderInputs
	Timeout    time.Duration
}

type operatorPlan struct {
	releaseName        string
	namespace          string
	timeout            time.Duration
	archive            []byte
	chart              *chart.Chart
	chartFingerprint   string
	expectedValuesJSON []byte
	valuesYAML         []byte
	expectedManifest   []byte
	renderedManifest   []byte
	postRenderer       postrender.PostRenderer
}

type operatorInstall struct {
	releaseName  string
	namespace    string
	timeout      time.Duration
	wait         bool
	chart        *chart.Chart
	values       map[string]interface{}
	postRenderer postrender.PostRenderer
}

type operatorActions interface {
	Get(ctx context.Context, name string) (*release.Release, error)
	CheckNoRenderedResources(ctx context.Context, manifest []byte) error
	Install(ctx context.Context, request operatorInstall) (*release.Release, error)
}

// EnsureOperator installs the retained DPF operator chart into the explicit
// management cluster, or accepts an already deployed release only when its
// chart content, values, and rendered manifest match this request exactly.
// It never upgrades, adopts, or removes an existing release.
func EnsureOperator(ctx context.Context, request OperatorRequest) error {
	if ctx == nil {
		return fmt.Errorf("operator context is required")
	}
	if request.RESTConfig == nil {
		return fmt.Errorf("management REST config is required")
	}
	if request.Timeout <= 0 {
		return fmt.Errorf("operator timeout must be positive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	operationCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()

	plan, err := prepareOperator(request)
	if err != nil {
		return err
	}
	if err := operationCtx.Err(); err != nil {
		return err
	}
	config, err := operatorRESTConfig(operationCtx, request.RESTConfig, request.Timeout)
	if err != nil {
		return err
	}
	actionConfig, err := helmclient.NewActionConfig(config, plan.namespace, helmclient.StorageDriver)
	if err != nil {
		return fmt.Errorf("initialize DPF Helm action config: %w", err)
	}
	return ensureOperator(operationCtx, plan, helmActionClient{config: actionConfig})
}

func prepareOperator(request OperatorRequest) (*operatorPlan, error) {
	archive := bytes.Clone(request.Archive)
	rendered, err := bootstrap.RenderOperator(archive, request.Inputs)
	if err != nil {
		return nil, fmt.Errorf("validate retained DPF operator chart and inputs: %w", err)
	}
	operatorChart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load retained DPF operator chart: %w", err)
	}
	if operatorChart.Metadata == nil || operatorChart.Metadata.Name == "" || operatorChart.Metadata.Version == "" {
		return nil, fmt.Errorf("retained DPF operator chart has incomplete metadata")
	}
	if len(operatorChart.Dependencies()) != 0 {
		return nil, fmt.Errorf("retained DPF operator chart has subcharts that Helm does not preserve in release metadata")
	}
	if err := validateInstallableChartShape(operatorChart); err != nil {
		return nil, err
	}
	fingerprint, err := chartFingerprint(operatorChart)
	if err != nil {
		return nil, fmt.Errorf("fingerprint retained DPF operator chart: %w", err)
	}
	values, err := helmclient.UnmarshalValues(rendered.HelmValues)
	if err != nil {
		return nil, fmt.Errorf("decode rendered DPF Helm values: %w", err)
	}
	valuesJSON, err := canonicalJSON(values)
	if err != nil {
		return nil, fmt.Errorf("canonicalize rendered DPF Helm values: %w", err)
	}
	manifest, err := canonicalManifest(rendered.Manifest)
	if err != nil {
		return nil, fmt.Errorf("canonicalize rendered DPF operator manifest: %w", err)
	}
	return &operatorPlan{
		releaseName:        request.Inputs.ReleaseName,
		namespace:          request.Inputs.Bootstrap.OperatorNamespace,
		timeout:            request.Timeout,
		archive:            archive,
		chart:              operatorChart,
		chartFingerprint:   fingerprint,
		expectedValuesJSON: valuesJSON,
		valuesYAML:         bytes.Clone(rendered.HelmValues),
		expectedManifest:   manifest,
		renderedManifest:   bytes.Clone(rendered.Manifest),
		postRenderer:       rendered.PostRenderer,
	}, nil
}

func ensureOperator(ctx context.Context, plan *operatorPlan, actions operatorActions) error {
	if ctx == nil || plan == nil || actions == nil {
		return fmt.Errorf("operator context, plan, and Helm actions are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	installed, err := actions.Get(ctx, plan.releaseName)
	if err == nil {
		if err := verifyOperatorRelease(installed, plan); err != nil {
			return fmt.Errorf("existing DPF Helm release conflicts with selected chart: %w", err)
		}
		return nil
	}
	if !errors.Is(err, driver.ErrReleaseNotFound) {
		return fmt.Errorf("inspect DPF Helm release %q: %w", plan.releaseName, safeAPIError("inspect DPF Helm release", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := actions.CheckNoRenderedResources(ctx, bytes.Clone(plan.renderedManifest)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("selected DPF chart resources are not safe to install")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	values, err := helmclient.UnmarshalValues(plan.valuesYAML)
	if err != nil {
		return fmt.Errorf("copy rendered DPF Helm values: %w", err)
	}
	installed, err = actions.Install(ctx, operatorInstall{
		releaseName:  plan.releaseName,
		namespace:    plan.namespace,
		timeout:      plan.timeout,
		wait:         true,
		chart:        plan.chart,
		values:       values,
		postRenderer: plan.postRenderer,
	})
	if err != nil {
		return fmt.Errorf("install selected DPF Helm release: %w", safeAPIError("install DPF Helm release", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyOperatorRelease(installed, plan); err != nil {
		return fmt.Errorf("installed DPF Helm release differs from selected chart: %w", err)
	}
	return nil
}

func verifyOperatorRelease(installed *release.Release, plan *operatorPlan) error {
	if installed == nil {
		return fmt.Errorf("helm returned no release")
	}
	if installed.Name != plan.releaseName || installed.Namespace != plan.namespace {
		return fmt.Errorf("release identity does not match the requested name and namespace")
	}
	if installed.Info == nil || installed.Info.Status != release.StatusDeployed {
		return fmt.Errorf("release is not in deployed status")
	}
	fingerprint, err := chartFingerprint(installed.Chart)
	if err != nil {
		return fmt.Errorf("read installed chart content: %w", err)
	}
	if fingerprint != plan.chartFingerprint {
		return fmt.Errorf("installed chart content differs")
	}
	valuesJSON, err := canonicalJSON(installed.Config)
	if err != nil || !bytes.Equal(valuesJSON, plan.expectedValuesJSON) {
		return fmt.Errorf("installed Helm values differ")
	}
	manifest, err := canonicalManifest([]byte(installed.Manifest))
	if err != nil || !bytes.Equal(manifest, plan.expectedManifest) {
		return fmt.Errorf("installed rendered manifest differs")
	}
	return nil
}

type helmActionClient struct {
	config *action.Configuration
}

func (c helmActionClient) Get(ctx context.Context, name string) (*release.Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return action.NewGet(c.config).Run(name)
}

// CheckNoRenderedResources rejects every existing object in the chart's
// rendered projection before Helm records or installs the release. Helm's
// normal install path adopts resources carrying matching release annotations,
// so absence is required even for objects that appear Helm-owned.
func (c helmActionClient) CheckNoRenderedResources(ctx context.Context, manifest []byte) error {
	if ctx == nil || c.config == nil || c.config.KubeClient == nil {
		return fmt.Errorf("context and Helm Kubernetes client are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resources, err := c.config.KubeClient.Build(bytes.NewReader(manifest), false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("cannot inspect rendered chart resources")
	}
	if len(resources) == 0 {
		return fmt.Errorf("selected DPF chart renders no Kubernetes resources")
	}
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if resource == nil || resource.Mapping == nil {
			return fmt.Errorf("rendered chart contains an unidentifiable resource")
		}
		if resource.Mapping.GroupVersionKind.Kind == "Namespace" {
			return fmt.Errorf("selected DPF chart must not manage namespaces")
		}
		if err := resource.Get(); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("cannot verify absence of a rendered chart resource")
		}
		return fmt.Errorf("a rendered DPF chart resource already exists")
	}
	return nil
}

func (c helmActionClient) Install(ctx context.Context, request operatorInstall) (*release.Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	installConfig := *c.config
	installConfig.KubeClient = createOnlyKubeClient{Interface: c.config.KubeClient}
	install := action.NewInstall(&installConfig)
	install.ReleaseName = request.releaseName
	install.Namespace = request.namespace
	install.CreateNamespace = false
	install.DependencyUpdate = false
	install.DisableHooks = false
	install.Replace = false
	install.Atomic = false
	install.Wait = request.wait
	install.Timeout = request.timeout
	install.PostRenderer = request.postRenderer
	install.TakeOwnership = false
	return install.RunWithContext(ctx, request.chart, request.values)
}

// createOnlyKubeClient prevents Helm's install path from adopting matching-
// ownership resources that appear after the preflight GETs. With this wrapper,
// Helm's empty-adoption path still uses Create, whose API operation rejects a
// concurrent name collision.
type createOnlyKubeClient struct {
	kube.Interface
}

func (createOnlyKubeClient) Update(kube.ResourceList, kube.ResourceList, bool) (*kube.Result, error) {
	return nil, fmt.Errorf("DPF operator install cannot update or adopt existing resources")
}

func validateInstallableChartShape(operatorChart *chart.Chart) error {
	if operatorChart == nil {
		return fmt.Errorf("retained DPF operator chart is required")
	}
	if len(operatorChart.CRDObjects()) != 0 {
		return fmt.Errorf("retained DPF operator chart CRDs require a separate reviewed installation path")
	}
	for _, template := range operatorChart.Templates {
		if template != nil && bytes.Contains(template.Data, []byte("helm.sh/hook")) {
			return fmt.Errorf("retained DPF operator chart hooks are not supported for no-adoption install")
		}
	}
	return nil
}

func operatorRESTConfig(ctx context.Context, original *rest.Config, timeout time.Duration) (*rest.Config, error) {
	if original == nil {
		return nil, fmt.Errorf("management REST config is required")
	}
	config := rest.CopyConfig(original)
	if config.Timeout <= 0 || timeout < config.Timeout {
		config.Timeout = timeout
	}
	if config.Transport != nil && config.WrapTransport != nil {
		return nil, fmt.Errorf("management REST config cannot set both Transport and WrapTransport")
	}
	if config.Transport != nil {
		config.Transport = contextRoundTripper{ctx: ctx, transport: config.Transport}
		return config, nil
	}
	wrap := config.WrapTransport
	config.WrapTransport = func(transport http.RoundTripper) http.RoundTripper {
		if wrap != nil {
			transport = wrap(transport)
		}
		return contextRoundTripper{ctx: ctx, transport: transport}
	}
	return config, nil
}

type contextRoundTripper struct {
	ctx       context.Context
	transport http.RoundTripper
}

func (t contextRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	finish := func() {
		stop()
		cancel()
	}
	response, err := t.transport.RoundTrip(request.Clone(requestCtx))
	if err != nil {
		finish()
		return response, err
	}
	if response == nil || response.Body == nil {
		finish()
		return response, nil
	}
	response.Body = &contextResponseBody{ReadCloser: response.Body, finish: finish}
	return response, nil
}

// contextResponseBody keeps the operation context alive while the caller
// consumes the response. RoundTripper returns at headers; client-go decodes
// the body afterward, so canceling in RoundTrip would truncate valid replies.
type contextResponseBody struct {
	io.ReadCloser
	once     sync.Once
	finish   func()
	closeErr error
}

func (b *contextResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		_ = b.close()
	}
	return n, err
}

func (b *contextResponseBody) Close() error {
	return b.close()
}

func (b *contextResponseBody) close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.finish()
	})
	return b.closeErr
}

func chartFingerprint(operatorChart *chart.Chart) (string, error) {
	if operatorChart == nil || operatorChart.Metadata == nil {
		return "", fmt.Errorf("chart metadata is required")
	}
	if len(operatorChart.Dependencies()) != 0 {
		return "", fmt.Errorf("chart dependencies are not supported for exact Helm release comparison")
	}
	h := sha256.New()
	if err := writeJSONHash(h, "metadata", operatorChart.Metadata); err != nil {
		return "", err
	}
	if err := writeJSONHash(h, "values", operatorChart.Values); err != nil {
		return "", err
	}
	if err := writeJSONHash(h, "lock", operatorChart.Lock); err != nil {
		return "", err
	}
	if err := writeHashPart(h, "schema", operatorChart.Schema); err != nil {
		return "", err
	}
	for _, files := range []struct {
		name string
		list []*chart.File
	}{{"templates", operatorChart.Templates}, {"files", operatorChart.Files}} {
		ordered := slices.Clone(files.list)
		for _, file := range ordered {
			if file == nil {
				return "", fmt.Errorf("chart %s contains an empty file entry", files.name)
			}
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
		if err := writeHashUint(h, uint64(len(ordered))); err != nil {
			return "", err
		}
		for _, file := range ordered {
			if err := writeHashPart(h, files.name+"/name", []byte(file.Name)); err != nil {
				return "", err
			}
			if err := writeHashPart(h, files.name+"/data", file.Data); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSONHash(h hash.Hash, label string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode chart %s: %w", label, err)
	}
	return writeHashPart(h, label, data)
}

func writeHashPart(h hash.Hash, label string, data []byte) error {
	if err := writeHashUint(h, uint64(len(label))); err != nil {
		return err
	}
	if _, err := h.Write([]byte(label)); err != nil {
		return err
	}
	if err := writeHashUint(h, uint64(len(data))); err != nil {
		return err
	}
	if _, err := h.Write(data); err != nil {
		return err
	}
	return nil
}

func writeHashUint(h hash.Hash, value uint64) error {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, err := h.Write(encoded[:])
	return err
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func canonicalManifest(manifest []byte) ([]byte, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	type document struct {
		identity string
		content  []byte
	}
	var documents []document
	seen := map[string]struct{}{}
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read manifest document %d: %w", ordinal, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var object unstructured.Unstructured
		if err := sigsyaml.Unmarshal(raw, &object.Object); err != nil {
			return nil, fmt.Errorf("decode manifest document %d: %w", ordinal, err)
		}
		if object.GetKind() == "Namespace" {
			return nil, fmt.Errorf("selected DPF operator chart must not manage namespaces")
		}
		key := documentIdentity(object.GroupVersionKind(), object.GetNamespace(), object.GetName())
		if key == "" {
			return nil, fmt.Errorf("manifest document %d lacks apiVersion, kind, or metadata.name", ordinal)
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("manifest contains duplicate resource %s", key)
		}
		seen[key] = struct{}{}
		content, err := canonicalJSON(object.Object)
		if err != nil {
			return nil, fmt.Errorf("canonicalize manifest document %d: %w", ordinal, err)
		}
		documents = append(documents, document{identity: key, content: content})
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].identity < documents[j].identity })
	return json.Marshal(documents)
}

func documentIdentity(gvk schema.GroupVersionKind, namespace, name string) string {
	if gvk.Version == "" || gvk.Kind == "" || name == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind, namespace, name)
}
