// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const bundleAnnotation = "nvidia.kubernetes-launch-kit.dpf-bundle-digest"
const desiredAnnotation = "nvidia.kubernetes-launch-kit.dpf-object-digest"

type run struct {
	runner            Runner
	result            Result
	bundle            *bundle.Bundle
	g                 config.Generation
	clients           map[bundle.Destination]Client
	namespaceUIDs     map[bundle.Destination]map[string]string
	selectedNodes     map[string]map[string]string
	clusterGeneration int64
}

// Run operates only on a frozen bundle and verified explicit cluster bindings.
// DryRun uses Kubernetes server-side dry-run for native documents; projections
// are validated/waited separately and never submitted as independent resources.
func (r Runner) Run(ctx context.Context, b *bundle.Bundle, g config.Generation, mode Mode) (Result, error) {
	if ctx == nil || b == nil {
		return Result{}, fmt.Errorf("DPF lifecycle requires context and a validated bundle")
	}
	if mode != Deploy && mode != Validate && mode != DryRun {
		return Result{}, fmt.Errorf("unsupported DPF lifecycle mode %q", mode)
	}
	if r.Timeout <= 0 {
		r.Timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if r.EvidenceDir == "" {
		return Result{}, fmt.Errorf("DPF lifecycle requires an explicit evidence directory outside the bundle")
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if err := os.MkdirAll(r.EvidenceDir, 0700); err != nil {
		return Result{}, err
	}
	runDir, err := os.MkdirTemp(r.EvidenceDir, "dpf-"+string(mode)+"-")
	if err != nil {
		return Result{}, err
	}
	if r.Clients == nil {
		r.Clients = NativeClients{}
	}
	if r.Artifacts == nil {
		r.Artifacts = CachedArtifacts{}
	}
	if r.PollInterval <= 0 {
		r.PollInterval = 2 * time.Second
	}
	s := &run{runner: r, bundle: b, g: g, result: Result{Mode: mode, BundleDigest: b.Index().BundleDigest, Outcome: "running", Checks: []Check{}, Events: []Event{}, ReportPath: filepath.Join(runDir, "report.json")}, clients: map[bundle.Destination]Client{}, namespaceUIDs: map[bundle.Destination]map[string]string{}}
	if err := s.persist(); err != nil {
		return s.result, err
	}
	err = s.execute(ctx)
	if err == nil && mode == DryRun {
		for _, check := range s.result.Checks {
			if check.State == "unresolved" {
				err = fail("DRY_RUN_INCOMPLETE", "one or more native API admission checks remain unresolved")
				break
			}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		s.result.Outcome = "failed"
		if reason(err) == "DRY_RUN_INCOMPLETE" {
			s.result.Outcome = "incomplete"
		}
		if ctx.Err() != nil {
			s.result.Outcome = "cancelled"
		}
		s.result.Checks = append(s.result.Checks, Check{ID: "execution", Subject: "bundle", Source: bundle.IndexPath, State: "failed", ReasonCode: reason(err), Message: err.Error()})
	} else {
		s.result.Outcome = "complete"
	}
	if persistErr := s.persist(); persistErr != nil {
		return s.result, fmt.Errorf("persist DPF lifecycle evidence: %w", persistErr)
	}
	return s.result, err
}

func (s *run) persist() error {
	data, err := json.MarshalIndent(s.result, "", "  ")
	if err != nil {
		return err
	}
	return artifacts.AtomicWrite(s.result.ReportPath, append(data, '\n'))
}

func (s *run) check(id, subject, source, state, code, message string) error {
	s.result.Checks = append(s.result.Checks, Check{ID: id, Subject: subject, Source: source, State: state, ReasonCode: code, Message: message})
	return s.persist()
}

func (s *run) event(stage, action string, reference bundle.ResourceRef, object *unstructured.Unstructured) error {
	e := Event{At: time.Now().UTC(), Stage: stage, Action: action, Resource: reference}
	if object != nil {
		e.UID = string(object.GetUID())
		e.ResourceVersion = object.GetResourceVersion()
	}
	s.result.Events = append(s.result.Events, e)
	return s.persist()
}

func (s *run) execute(ctx context.Context) error {
	i := s.bundle.Index()
	if i.Profile != s.g.Profile || strings.TrimPrefix(i.Release, "v") != s.g.Release {
		return fail("BUNDLE_INTENT_MISMATCH", "bundle profile/release differs from resolved intent")
	}
	want := map[string]bool{}
	for _, binding := range s.g.Provisioning.Devices {
		want[binding.ID] = true
	}
	if len(want) != len(i.SelectedDevices) {
		return fail("BUNDLE_SCOPE_MISMATCH", "bundle selected-device count differs")
	}
	for _, id := range i.SelectedDevices {
		if !want[id] {
			return fail("BUNDLE_SCOPE_MISMATCH", "bundle selected-device identity differs")
		}
	}
	management, err := s.runner.Clients.Management(ctx, s.g)
	if err != nil {
		return fail("MANAGEMENT_ACCESS_FAILED", err.Error())
	}
	s.clients[bundle.Management] = management
	if err = s.identity(ctx, management, s.g.ManagementUID, "management"); err != nil {
		return err
	}
	if err = s.management(ctx); err != nil {
		return err
	}
	if err = s.cluster(ctx); err != nil {
		return err
	}
	tenant, err := s.runner.Clients.Tenant(ctx, management, s.g.Provisioning.DPUCluster)
	if err != nil {
		return fail("TENANT_ACCESS_FAILED", err.Error())
	}
	s.clients[bundle.DPU] = tenant
	if err = s.identity(ctx, tenant, s.g.Provisioning.DPUCluster.ClusterUID, "tenant"); err != nil {
		return err
	}
	if err = s.selection(ctx); err != nil {
		return err
	}
	if err = s.externalSecrets(ctx); err != nil {
		return err
	}
	if s.result.Mode == DryRun {
		if err := s.check("runtime-readiness", "tenant DNS and selected DPU Nodes", "runtime", "not-evaluated", "RUNTIME_READINESS_NOT_EVALUATED", "server-side admission only; workload readiness requires persistent provisioning"); err != nil {
			return err
		}
	}
	for _, stage := range i.Stages {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, id := range stage.Requires {
			switch id {
			case "existing-management", "existing-tenant", "enrolled-devices", "selected-scope", "external-transition-complete", "existing-secrets":
			case "software-integrity": // Enforced immediately before every DPUSet below.
			default:
				return fail("EXTERNAL_PREREQUISITE_UNSUPPORTED", "unknown prerequisite "+id)
			}
		}
		view, _ := s.bundle.Stage(stage.ID)
		for _, doc := range view.Documents() {
			if err := s.document(ctx, stage, doc); err != nil {
				return err
			}
			// A DPUSet starts a physical operation. Finish it before the next
			// document rechecks the complete selected-device/BMC domain; those
			// prerequisites may be unavailable during the operation just started.
			if s.result.Mode == Deploy && doc.Ref().Kind == "DPUSet" {
				if err := s.wait(ctx, stage.ID, doc); err != nil {
					return err
				}
			}
		}
		if s.result.Mode != DryRun {
			for _, doc := range append(view.Documents(), view.Projections()...) {
				if s.result.Mode == Deploy && doc.Ref().Kind == "DPUSet" && !doc.IsProjection() {
					continue // Already waited before processing the next document.
				}
				if err := s.wait(ctx, stage.ID, doc); err != nil {
					return err
				}
			}
			for _, doc := range view.Documents() {
				if doc.Ref().Kind == "DPUSet" {
					if err := s.selectedReady(ctx); err != nil {
						return err
					}
					if err := s.baseWorkloads(ctx); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	if s.result.Mode != DryRun {
		if err := s.selectedReady(ctx); err != nil {
			return err
		}
		if err := s.baseWorkloads(ctx); err != nil {
			return err
		}
		// DNS may take time to converge; sample node transitions again afterward.
		return s.selectedReady(ctx)
	}
	return nil
}

func (s *run) identity(ctx context.Context, c Client, expected, subject string) error {
	if expected == "" {
		return fail("CLUSTER_IDENTITY_MISSING", subject+" kube-system UID is unresolved")
	}
	object, err := c.Get(ctx, ref("", "", "Namespace", "", "kube-system"))
	if err != nil {
		return fail("CLUSTER_IDENTITY_UNAVAILABLE", subject+" kube-system cannot be read")
	}
	if string(object.GetUID()) != expected {
		return fail("CLUSTER_IDENTITY_MISMATCH", subject+" kube-system UID differs")
	}
	expectedVersion := s.g.ManagementKubernetesVersion
	if subject == "tenant" || subject == string(bundle.DPU) {
		expectedVersion = s.g.Provisioning.DPUCluster.KubernetesVersion
	}
	version, err := c.ServerVersion(ctx)
	if err != nil || expectedVersion == "" {
		return fail("KUBERNETES_VERSION_UNOBSERVED", subject+" API server version must be bound")
	}
	if strings.TrimPrefix(version, "v") != strings.TrimPrefix(expectedVersion, "v") {
		return fail("KUBERNETES_VERSION_DRIFT", subject+" API server version differs from frozen Helm projection; regenerate")
	}
	return s.check("cluster-identity", subject, "kubernetes:Namespace/kube-system", "passed", "IDENTITY_MATCH", "")
}

func (s *run) document(ctx context.Context, stage bundle.Stage, doc bundle.Document) error {
	c := s.clients[stage.Destination]
	object := doc.ObjectCopy()
	reference := doc.Ref()
	if reference.Kind == "DPUDevice" || reference.Kind == "DPUNode" || reference.Kind == "DPUDiscovery" || reference.Kind == "DPFOperatorConfig" || reference.Kind == "DPUCluster" {
		return fail("EXTERNAL_OWNERSHIP_REQUIRED", "first lifecycle slice cannot create enrollment/operator/tenant resources")
	}
	if _, ok := nativeBody(s.g.Release, object); !ok {
		return fail("NATIVE_KIND_UNSUPPORTED", fmt.Sprintf("no native comparison contract for %s %s", object.GetAPIVersion(), object.GetKind()))
	}
	current, err := c.Get(ctx, reference)
	if err == nil {
		if !owns(current, object, s.result.BundleDigest, s.g.Release) {
			return fail("RESOURCE_OWNERSHIP_CONFLICT", fmt.Sprintf("existing %s/%s is not this bundle's unchanged object", reference.Kind, reference.Name))
		}
		return s.event(stage.ID, "unchanged", reference, current)
	}
	if !apierrors.IsNotFound(err) {
		if s.result.Mode == DryRun && meta.IsNoMatchError(err) {
			return s.check("admission", reference.Kind+"/"+reference.Name, doc.Source().File, "unresolved", "API_NOT_SERVED", "server-side dry-run deferred until the declared API exists")
		}
		return fail("RESOURCE_READ_FAILED", fmt.Sprintf("cannot inspect %s/%s", reference.Kind, reference.Name))
	}
	if s.result.Mode == Validate {
		if s.runner.Wait {
			return nil
		}
		return fail("RESOURCE_MISSING", fmt.Sprintf("desired %s/%s is absent", reference.Kind, reference.Name))
	}
	if reference.Kind == "DPUSet" && s.result.Mode != DryRun {
		if err := s.selection(ctx); err != nil {
			return err
		}
		if err := s.provisioningGate(ctx, object); err != nil {
			return err
		}
	}
	marked := mark(object, s.result.BundleDigest)
	expectedUID := s.g.ManagementUID
	if stage.Destination == bundle.DPU {
		expectedUID = s.g.Provisioning.DPUCluster.ClusterUID
	}
	if err := s.identity(ctx, c, expectedUID, string(stage.Destination)); err != nil {
		return err
	}
	if reference.Namespace != "" {
		if pinned := s.namespaceUIDs[stage.Destination][reference.Namespace]; pinned != "" {
			namespace, err := c.Get(ctx, ref(stage.Destination, "", "Namespace", "", reference.Namespace))
			if err != nil || string(namespace.GetUID()) != pinned {
				return fail("NAMESPACE_IDENTITY_MISMATCH", "namespace changed before mutation")
			}
		}
	}
	if s.result.Mode == Deploy && len(s.selectedNodes) > 0 {
		if _, err := s.tenantScope(ctx, s.selectedNodes); err != nil {
			return err
		}
		clear, diagnostic, err := s.selectedTransitionsClear(ctx)
		if err != nil {
			return err
		}
		if !clear {
			return fail("NODE_TRANSITION_PENDING", diagnostic)
		}
	}
	if reference.Kind == "DPUSet" && s.result.Mode == Deploy {
		// Admission may introduce additional physical settings. Inspect the
		// admitted spec without persistence before requesting a real DPUSet.
		if err := s.event(stage.ID, "admission-precheck-requested", reference, nil); err != nil {
			return err
		}
		admitted, err := c.Create(ctx, marked, true)
		if err != nil {
			return fail("ADMISSION_PRECHECK_FAILED", fmt.Sprintf("server-side admission precheck rejected %s/%s: %s", reference.Kind, reference.Name, reason(err)))
		}
		if !nativeMatches(s.g.Release, object, admitted) {
			return fail("ADMISSION_CHANGED_DESIRED_STATE", "admission introduced unexpected DPUSet desired state before persistent creation")
		}
		if err := s.event(stage.ID, "admission-precheck-passed", reference, admitted); err != nil {
			return err
		}
		// Neither cache verification nor API admission is instantaneous.
		// Retain the DPUCluster spec snapshot at the physical create boundary.
		if err := s.cluster(ctx); err != nil {
			return err
		}
	}
	action := "create-requested"
	if s.result.Mode == DryRun {
		action = "dry-run-requested"
	}
	if err := s.event(stage.ID, action, reference, nil); err != nil {
		return err
	}
	created, err := c.Create(ctx, marked, s.result.Mode == DryRun)
	if err != nil {
		if s.result.Mode == DryRun && apierrors.IsNotFound(err) {
			return s.check("admission", reference.Kind+"/"+reference.Name, doc.Source().File, "unresolved", "ADMISSION_PREREQUISITE_UNRESOLVED", "server-side dry-run requires an API or namespace that is not yet served")
		}
		return fail("RESOURCE_CREATE_FAILED", fmt.Sprintf("API rejected %s/%s: %s", reference.Kind, reference.Name, reason(err)))
	}
	if !nativeMatches(s.g.Release, object, created) {
		return fail("ADMISSION_CHANGED_DESIRED_STATE", fmt.Sprintf("admission changed supplied fields on %s/%s", reference.Kind, reference.Name))
	}
	action = "created"
	if s.result.Mode == DryRun {
		action = "dry-run-admitted"
	}
	return s.event(stage.ID, action, reference, created)
}

func (s *run) wait(ctx context.Context, stage string, doc bundle.Document) error {
	for {
		object, err := s.clients[doc.Ref().Destination].Get(ctx, doc.Ref())
		if err != nil && !apierrors.IsNotFound(err) {
			return fail("READINESS_READ_FAILED", fmt.Sprintf("cannot read %s/%s readiness", doc.Ref().Kind, doc.Ref().Name))
		}
		if object != nil {
			if doc.IsProjection() {
				if !desiredMatches(doc.ObjectCopy(), object) {
					return fail("PROJECTION_DRIFT", doc.Ref().Kind+"/"+doc.Ref().Name+" differs from its frozen workload projection")
				}
			} else if !owns(object, doc.ObjectCopy(), s.result.BundleDigest, s.g.Release) {
				return fail("RESOURCE_OWNERSHIP_CONFLICT", doc.Ref().Kind+"/"+doc.Ref().Name+" changed while waiting")
			}
		}
		ready, terminal, message := Readiness(object)
		if ready && doc.Ref().Kind == "DPUSet" {
			// Native Ready can precede removal of the host reset marker.
			// Do not start another operation until the selected domain is idle.
			ready, message, err = s.selectedTransitionsClear(ctx)
			if err != nil {
				return err
			}
		}
		if ready && doc.IsProjection() && doc.Ref().Destination == bundle.DPU && doc.Ref().Kind == "DaemonSet" {
			ready, message, err = s.daemonSetCoverage(ctx, object)
			if err != nil {
				return err
			}
		}
		if ready {
			return s.check("readiness", doc.Ref().Kind+"/"+doc.Ref().Name, doc.Source().File, "passed", "READY", message)
		}
		if terminal {
			return fail("RESOURCE_NOT_READY", message)
		}
		if doc.Ref().Kind == "DPUSet" {
			diagnostics, err := s.selectedTransitionDiagnostics(ctx)
			if err != nil {
				return err
			}
			message += "; " + diagnostics
		}
		if s.result.Mode == Validate && !s.runner.Wait {
			return fail("RESOURCE_NOT_READY", doc.Ref().Kind+"/"+doc.Ref().Name+": "+message)
		}
		if err := s.check("readiness", doc.Ref().Kind+"/"+doc.Ref().Name, doc.Source().File, "pending", "WAITING_FOR_RECONCILIATION", message); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.runner.PollInterval):
		}
	}
}

func mark(object *unstructured.Unstructured, digest string) *unstructured.Unstructured {
	copy := object.DeepCopy()
	annotations := copy.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[bundleAnnotation] = digest
	annotations[desiredAnnotation] = objectDigest(object)
	copy.SetAnnotations(annotations)
	return copy
}

func objectDigest(object *unstructured.Unstructured) string {
	data, _ := json.Marshal(object.Object)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func owns(current, desired *unstructured.Unstructured, digest, release string) bool {
	return current != nil && desired != nil && current.GetAnnotations()[bundleAnnotation] == digest && current.GetAnnotations()[desiredAnnotation] == objectDigest(desired) && nativeMatches(release, desired, current)
}

type failure struct{ code, message string }

func (e *failure) Error() string      { return e.code + ": " + e.message }
func fail(code, message string) error { return &failure{code: code, message: message} }
func reason(err error) string {
	if f, ok := err.(*failure); ok {
		return f.code
	}
	if apierrors.IsForbidden(err) {
		return "ACCESS_DENIED"
	}
	if apierrors.IsAlreadyExists(err) {
		return "ALREADY_EXISTS"
	}
	if apierrors.IsNotFound(err) {
		return "NOT_FOUND"
	}
	if err == context.Canceled {
		return "CANCELLED"
	}
	if err == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "OPERATION_FAILED"
}

func ref(destination bundle.Destination, group, kind, namespace, name string) bundle.ResourceRef {
	return bundle.ResourceRef{Destination: destination, Group: group, Kind: kind, Namespace: namespace, Name: name}
}
func text(o *unstructured.Unstructured, fields ...string) string {
	if o == nil {
		return ""
	}
	value, _, _ := unstructured.NestedString(o.Object, fields...)
	return value
}
func number(o *unstructured.Unstructured, fields ...string) int64 {
	if o == nil {
		return 0
	}
	value, _, _ := unstructured.NestedInt64(o.Object, fields...)
	return value
}
