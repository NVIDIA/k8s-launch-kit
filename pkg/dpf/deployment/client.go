// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	dpfdiscovery "github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// APIClient binds native operations to one explicitly configured management
// cluster. It offers no update, delete, adoption, or rollback operation. The
// lifecycle caller owns stage prerequisites and resource ownership decisions.
type APIClient struct {
	access   dpfdiscovery.ManagementClient
	dynamic  dynamic.Interface
	expected generation.ManagementIdentity
	version  string
}

// NewAPIClient snapshots explicit kubeconfig access and checks the generated
// cluster binding. Ambient kubeconfig and executable auth plugins are rejected
// by the same access loader used during discovery.
func NewAPIClient(ctx context.Context, request config.Request, expected generation.ManagementIdentity, version string) (*APIClient, error) {
	if expected.APIEndpoint == "" || expected.SystemNamespaceUID == "" || expected.TrustDigest == "" || version == "" {
		return nil, fmt.Errorf("complete generated management identity and Kubernetes version are required")
	}
	if request.RequestTimeout <= 0 {
		request.RequestTimeout = 30 * time.Second
	}
	access, err := dpfdiscovery.ManagementAccess(request)
	if err != nil {
		return nil, err
	}
	d, err := dynamic.NewForConfig(access.REST)
	if err != nil {
		return nil, fmt.Errorf("cannot construct bound management resource client")
	}
	c := &APIClient{access: access, dynamic: d, expected: expected, version: version}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// VerifyIdentity refreshes the cluster UID and server version through the same
// credential/trust snapshot. It must also precede Helm and effect-root actions.
func (c *APIClient) VerifyIdentity(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("management client is unavailable")
	}
	observed, err := c.access.ObserveIdentity(ctx)
	if err != nil {
		return err
	}
	if observed.APIEndpoint != c.expected.APIEndpoint || observed.SystemNamespaceUID != c.expected.SystemNamespaceUID || observed.TrustDigest != c.expected.TrustDigest {
		return fmt.Errorf("management cluster identity or TLS trust differs from the generated bundle; repeat discovery and generation")
	}
	if observed.KubernetesVersion != c.version {
		return fmt.Errorf("management Kubernetes version differs from the generated bundle; repeat discovery and generation")
	}
	return nil
}

// Get reads the exact GVK/name/namespace supplied by the retained desired object.
func (c *APIClient) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resource, err := c.resource(ctx, desired, true)
	if err != nil {
		return nil, err
	}
	object, err := resource.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, safeAPIError("read resource", err)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return object, nil
}

// Create sends a defensive copy only after refreshing management identity.
// AlreadyExists is returned to the lifecycle caller for exact ownership/content
// verification; it never authorizes replacement. No cleanup runs on failure.
func (c *APIClient) Create(ctx context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resource, err := c.resource(ctx, desired, true)
	if err != nil {
		return nil, err
	}
	if desired.GetUID() != "" || desired.GetResourceVersion() != "" || desired.GetGenerateName() != "" || desired.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("desired resource must have a fixed identity without server metadata")
	}
	if _, found := desired.Object["status"]; found {
		return nil, fmt.Errorf("desired resource must not contain status")
	}
	options := metav1.CreateOptions{FieldManager: "k8s-launch-kit-dpf"}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	object, err := resource.Create(ctx, desired.DeepCopy(), options)
	if err != nil {
		return nil, safeAPIError("create resource", err)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return object, nil
}

// List uses bounded, complete pagination. Partial results are never returned as
// a complete ownership or selector-domain observation.
func (c *APIClient) List(ctx context.Context, template *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resource, err := c.resource(ctx, template, false)
	if err != nil {
		return nil, err
	}
	var objects []unstructured.Unstructured
	cursor := ""
	seen := map[string]bool{}
	for range 16 {
		page, err := resource.List(ctx, metav1.ListOptions{Limit: 256, Continue: cursor, LabelSelector: selector})
		if err != nil {
			return nil, safeAPIError("list resources", err)
		}
		if len(page.Items) > 256 {
			return nil, fmt.Errorf("resource listing exceeded the requested page bound")
		}
		objects = append(objects, page.Items...)
		cursor = page.GetContinue()
		if cursor == "" {
			if err := c.VerifyIdentity(ctx); err != nil {
				return nil, err
			}
			return objects, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("resource listing repeated a continuation token")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("resource listing exceeds the complete observation limit")
}

func (c *APIClient) resource(ctx context.Context, object *unstructured.Unstructured, requireName bool) (dynamic.ResourceInterface, error) {
	if c == nil || c.dynamic == nil || c.access.Client == nil || object == nil {
		return nil, fmt.Errorf("bound client and desired resource are required")
	}
	gvk := object.GroupVersionKind()
	namespaced, known := bundle.ResourceNamespaced(gvk.Group, gvk.Kind)
	if !known || gvk.Version == "" || (requireName && object.GetName() == "") {
		return nil, fmt.Errorf("resource requires a reviewed kind, API version and explicit identity")
	}
	if (!namespaced && object.GetNamespace() != "") || (namespaced && requireName && object.GetNamespace() == "") {
		return nil, fmt.Errorf("resource namespace does not match the reviewed kind scope")
	}
	// Discovery is context-bound and refreshed, so a CRD installed by an earlier
	// stage cannot be hidden by a stale RESTMapper cache.
	apiPath := "/apis/" + gvk.GroupVersion().String()
	if gvk.Group == "" {
		apiPath = "/api/" + gvk.Version
	}
	data, err := c.access.Client.Discovery().RESTClient().Get().AbsPath(apiPath).Do(ctx).Raw()
	if err != nil {
		return nil, safeAPIError("discover resource API", err)
	}
	var resources metav1.APIResourceList
	if json.Unmarshal(data, &resources) != nil || resources.GroupVersion != gvk.GroupVersion().String() {
		return nil, fmt.Errorf("resource discovery returned an invalid group/version")
	}
	var selected *metav1.APIResource
	for i := range resources.APIResources {
		r := &resources.APIResources[i]
		if r.Kind != gvk.Kind || strings.Contains(r.Name, "/") {
			continue
		}
		if selected != nil || r.Namespaced != namespaced || r.Name == "" {
			return nil, fmt.Errorf("resource API kind mapping is ambiguous or has conflicting scope")
		}
		selected = r
	}
	if selected == nil {
		return nil, fmt.Errorf("required resource API is not served")
	}
	endpoint := c.dynamic.Resource(schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: selected.Name})
	if namespaced {
		return endpoint.Namespace(object.GetNamespace()), nil
	}
	return endpoint, nil
}

// Kubernetes errors may echo submitted object fields, including Secret data.
// Preserve machine-readable error classification without retaining server text.
func safeAPIError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	status := metav1.Status{Status: metav1.StatusFailure, Reason: apierrors.ReasonForError(err)}
	var source apierrors.APIStatus
	if errors.As(err, &source) {
		status.Code = source.Status().Code
	}
	status.Message = fmt.Sprintf("%s failed (%s)", operation, status.Reason)
	return &apierrors.StatusError{ErrStatus: status}
}
