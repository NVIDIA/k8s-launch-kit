// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

func scopePolicy(selector map[string]string, ids ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "sriovnetwork.openshift.io/v1", "kind": "SriovNetworkNodePolicy",
		"metadata": map[string]interface{}{"name": "policy", "namespace": "networking"},
	}}
	_ = unstructured.SetNestedStringMap(obj.Object, selector, "spec", "nodeSelector")
	if ids != nil {
		data, _ := json.Marshal(ids)
		obj.SetAnnotations(map[string]string{SourceGroupsAnnotation: string(data)})
	}
	return obj
}

func scopeConfig(groups ...config.ClusterConfig) *config.LaunchKitConfig {
	return &config.LaunchKitConfig{ClusterConfig: groups}
}
func scopeGroup(id string, workers ...string) config.ClusterConfig {
	return config.ClusterConfig{Identifier: id, WorkerNodes: workers}
}
func scopeNode(name string, nodeLabels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
}

func scopeBundle(obj *unstructured.Unstructured, withValues bool) *bundle.Bundle {
	content, err := yaml.Marshal(obj.Object)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	files := []bundle.File{{Name: "40-resource.yaml", Content: string(content)}}
	if withValues {
		files = append(files, bundle.File{Name: "values.yaml", Content: "operator: {}\n"})
	}
	artifacts, err := bundle.FromFiles(files)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return artifacts
}

var _ = ginkgo.Describe("hardware target scope", func() {
	var ctx context.Context
	var workers []corev1.Node
	var cfg *config.LaunchKitConfig
	ginkgo.BeforeEach(func() {
		ctx = context.Background()
		workers = []corev1.Node{
			scopeNode("gpu-a", map[string]string{"cohort": "gpu", "pci-15b3": "true", "node-role.kubernetes.io/control-plane": ""}),
			scopeNode("gpu-b", map[string]string{"cohort": "gpu", "pci-15b3": "true"}),
			scopeNode("dpu-control", map[string]string{"pci-15b3": "true"}),
		}
		cfg = scopeConfig(scopeGroup("source", "gpu-a", "gpu-b"))
	})
	ginkgo.It("rejects broad PCI selectors while preserving intended control-plane GPU workers", func() {
		obj := scopePolicy(map[string]string{"pci-15b3": "true"}, "source")
		scope, err := resolveTargetScope(obj, cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = checkTargetScope(obj, scope, workers)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		_ = unstructured.SetNestedStringMap(obj.Object, map[string]string{"cohort": "gpu"}, "spec", "nodeSelector")
		message, err := checkTargetScope(obj, scope, workers)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(message).To(gomega.ContainSubstring("verified"))
		_, err = checkTargetScope(obj, scope, workers[1:])
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("missing=[gpu-a]")))
	})
	ginkgo.DescribeTable("resolves explicit compatibility branches", func(ids []string, groups []config.ClusterConfig, want string, verified bool) {
		scope, err := resolveTargetScope(scopePolicy(nil, ids...), scopeConfig(groups...))
		if want != "" {
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(want)))
			return
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(scope.workers != nil).To(gomega.Equal(verified))
	},
		ginkgo.Entry("annotated inventory", []string{"s"}, []config.ClusterConfig{scopeGroup("s", "node")}, "", true),
		ginkgo.Entry("annotated unnamed sole source", []string{""}, []config.ClusterConfig{scopeGroup("", "node")}, "", true),
		ginkgo.Entry("annotated unnamed sole preset", []string{""}, []config.ClusterConfig{scopeGroup("")}, "", false),
		ginkgo.Entry("annotated unnamed without config", []string{""}, nil, "exactly one unnamed", false),
		ginkgo.Entry("annotated unnamed with named config", []string{""}, []config.ClusterConfig{scopeGroup("named", "node")}, "exactly one unnamed", false),
		ginkgo.Entry("annotated unnamed among multiple sources", []string{""}, []config.ClusterConfig{scopeGroup("", "node"), scopeGroup("other", "other")}, "exactly one unnamed", false),
		ginkgo.Entry("annotated unnamed alongside other references", []string{"", "named"}, []config.ClusterConfig{scopeGroup("", "node")}, "exactly one unnamed", false),
		ginkgo.Entry("whitespace source reference", []string{" "}, []config.ClusterConfig{scopeGroup(" ", "node")}, "source group ID", false),
		ginkgo.Entry("annotated preset", []string{"s"}, []config.ClusterConfig{scopeGroup("s")}, "", false),
		ginkgo.Entry("annotated missing configuration", []string{"s"}, nil, "missing", false),
		ginkgo.Entry("unknown source", []string{"missing"}, []config.ClusterConfig{scopeGroup("s", "node")}, "missing", false),
		ginkgo.Entry("duplicate source reference", []string{"s", "s"}, []config.ClusterConfig{scopeGroup("s", "node")}, "duplicate", false),
		ginkgo.Entry("duplicate configured source", []string{"s"}, []config.ClusterConfig{scopeGroup("s", "node"), scopeGroup("s", "other")}, "duplicate", false),
		ginkgo.Entry("duplicate worker", []string{"s", "t"}, []config.ClusterConfig{scopeGroup("s", "node"), scopeGroup("t", "node")}, "duplicate", false),
		ginkgo.Entry("empty worker", []string{"s"}, []config.ClusterConfig{scopeGroup("s", "")}, "malformed", false),
		ginkgo.Entry("partial intent", []string{"s", "t"}, []config.ClusterConfig{scopeGroup("s", "node"), scopeGroup("t")}, "mix", false),
		ginkgo.Entry("legacy single source", nil, []config.ClusterConfig{scopeGroup("s", "node")}, "", true),
		ginkgo.Entry("legacy unnamed single source", nil, []config.ClusterConfig{scopeGroup("", "node")}, "", true),
		ginkgo.Entry("legacy single preset", nil, []config.ClusterConfig{scopeGroup("s")}, "", false),
		ginkgo.Entry("legacy ambiguous inventory", nil, []config.ClusterConfig{scopeGroup("s", "node"), scopeGroup("t", "other")}, "ambiguous", false),
		ginkgo.Entry("legacy all preset", nil, []config.ClusterConfig{scopeGroup("s"), scopeGroup("t")}, "", false),
		ginkgo.Entry("legacy artifact only", nil, nil, "", false),
	)
	ginkgo.It("rejects malformed annotation rather than treating it as legacy", func() {
		obj := scopePolicy(nil)
		obj.SetAnnotations(map[string]string{SourceGroupsAnnotation: "[]"})
		_, err := resolveTargetScope(obj, cfg)
		gomega.Expect(err).To(gomega.HaveOccurred())
		obj.SetAnnotations(map[string]string{SourceGroupsAnnotation: "source"})
		_, err = resolveTargetScope(obj, cfg)
		gomega.Expect(err).To(gomega.HaveOccurred())
	})
	ginkgo.It("evaluates IPPool terms as OR and requirements within each term as AND", func() {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "nv-ipam.nvidia.com/v1alpha1", "kind": "IPPool", "metadata": map[string]interface{}{"name": "pool"},
			"spec": map[string]interface{}{"nodeSelector": map[string]interface{}{"nodeSelectorTerms": []interface{}{
				map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "cohort", "operator": "In", "values": []interface{}{"gpu"}}, map[string]interface{}{"key": "pci-15b3", "operator": "Exists"}}},
				map[string]interface{}{"matchFields": []interface{}{map[string]interface{}{"key": "metadata.name", "operator": "In", "values": []interface{}{"dpu-control"}}}},
			}}},
		}}
		scope := targetScope{workers: []string{"gpu-a", "gpu-b", "dpu-control"}}
		_, err := checkTargetScope(obj, scope, workers)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{}, "spec", "nodeSelector", "nodeSelectorTerms")
		_, err = checkTargetScope(obj, scope, workers)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("missing=")))
	})
	ginkgo.It("rejects deployment before Helm, patch or deletion even with overwrite", func() {
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		writes, installs := 0, 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				writes++
				return nil
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				writes++
				return nil
			},
		}).Build()
		opts := DeployOptions{Config: cfg, OverwriteExisting: true, RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{HelmRepoURL: "https://example.invalid", Version: "v1"}}
		installer := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
			installs++
			return nil
		}
		err := applyBundleWithInstaller(ctx, c, scopeBundle(scopePolicy(map[string]string{"pci-15b3": "true"}, "source"), true), opts, installer)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		gomega.Expect(writes).To(gomega.BeZero())
		gomega.Expect(installs).To(gomega.BeZero())
	})
	ginkgo.It("checks live selector drift against intended metadata in validation and deploy polling", func() {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		live := scopePolicy(map[string]string{"pci-15b3": "true"}, "attacker")
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, &workers[0], &workers[1], &workers[2]).Build()
		results, err := ValidateBundleWithConfig(ctx, c, scopeBundle(desired, false), cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(results).To(gomega.HaveLen(1))
		gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateError))
		gomega.Expect(results[0].Reason).To(gomega.ContainSubstring("extra=[dpu-control]"))
		err = pollUntilTerminal(ctx, c, crstate.NewDefault(), desired, "policy", "", cfg)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
	})
	ginkgo.DescribeTable("retries temporary scope reads before evaluating readiness", func(pool, nodeRead bool, readErr error) {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		if pool {
			desired.SetAPIVersion("nv-ipam.nvidia.com/v1alpha1")
			desired.SetKind("IPPool")
			gomega.Expect(unstructured.SetNestedMap(desired.Object, map[string]interface{}{"nodeSelectorTerms": []interface{}{
				map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "cohort", "operator": "In", "values": []interface{}{"gpu"}}}},
			}}, "spec", "nodeSelector")).To(gomega.Succeed())
		}
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		reads, validations, writes := 0, 0, 0
		failOnce := func() error {
			reads++
			if reads == 1 {
				return readErr
			}
			return nil
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired, &workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if !nodeRead {
					if err := failOnce(); err != nil {
						return err
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if nodeRead {
					if err := failOnce(); err != nil {
						return err
					}
				}
				return c.List(ctx, list, opts...)
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				writes++
				return nil
			},
		}).Build()
		registry := crstate.NewRegistry()
		registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
			gomega.Expect(reads).To(gomega.Equal(2), "unverified scope must not reach readiness")
			validations++
			return crstate.Result{State: crstate.StateSuccess}, nil
		})
		pollCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		err := pollUntilTerminalWithRetryableErrorTimeout(pollCtx, c, registry, desired, "resource", "", 0, time.Millisecond, cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(validations).To(gomega.Equal(1))
		gomega.Expect(writes).To(gomega.BeZero())
	},
		ginkgo.Entry("policy GET timeout", false, false, apierrors.NewTimeoutError("temporary read", 1)),
		ginkgo.Entry("policy node-list throttle", false, true, apierrors.NewTooManyRequests("busy", 1)),
		ginkgo.Entry("pool GET throttle", true, false, apierrors.NewTooManyRequests("busy", 1)),
		ginkgo.Entry("pool node-list timeout", true, true, apierrors.NewTimeoutError("temporary read", 1)),
		ginkgo.Entry("API server timeout", false, false, apierrors.NewServerTimeout(schema.GroupResource{Resource: "nodes"}, "list", 1)),
		ginkgo.Entry("API unavailable", true, true, apierrors.NewServiceUnavailable("temporarily unavailable")),
		ginkgo.Entry("internal API failure", false, true, apierrors.NewInternalError(fmt.Errorf("temporary server failure"))),
		ginkgo.Entry("request deadline", true, false, context.DeadlineExceeded),
		ginkgo.Entry("closed transport", false, false, fmt.Errorf("transport read: %w", io.ErrUnexpectedEOF)),
		ginkgo.Entry("connection reset", false, false, &net.OpError{Op: "read", Err: syscall.ECONNRESET}),
		ginkgo.Entry("connection refused", true, true, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}),
		ginkgo.Entry("HTTP2 connection lost", false, false, fmt.Errorf("http2: client connection lost")),
	)
	ginkgo.It("does not retry semantic scope errors containing transport wording", func() {
		gomega.Expect(isRetryableTargetScopeReadError(fmt.Errorf("source http2: client connection lost is missing"))).To(gomega.BeFalse())
		gomega.Expect(isRetryableTargetScopeReadError(fmt.Errorf("selector mismatch: connection reset by peer"))).To(gomega.BeFalse())
	})
	ginkgo.DescribeTable("keeps permanent scope read failures blocking", func(nodeRead bool, readErr error) {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		reads, validations := 0, 0
		c := fake.NewClientBuilder().WithObjects(desired).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if !nodeRead {
					reads++
					return readErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				reads++
				return readErr
			},
		}).Build()
		registry := crstate.NewRegistry()
		registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
			validations++
			return crstate.Result{State: crstate.StateSuccess}, nil
		})
		err := pollUntilTerminalWithRetryableErrorTimeout(ctx, c, registry, desired, "resource", "", 0, time.Millisecond, cfg)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(reads).To(gomega.Equal(1))
		gomega.Expect(validations).To(gomega.BeZero())
	},
		ginkgo.Entry("forbidden GET", false, apierrors.NewForbidden(schema.GroupResource{Resource: "policies"}, "policy", fmt.Errorf("denied"))),
		ginkgo.Entry("forbidden nodes", true, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", fmt.Errorf("denied"))),
		ginkgo.Entry("unauthorized", false, apierrors.NewUnauthorized("denied")),
		ginkgo.Entry("invalid request", true, apierrors.NewBadRequest("invalid request")),
		ginkgo.Entry("unclassified failure", false, fmt.Errorf("malformed response")),
	)
	ginkgo.It("checks scope again after a temporary read rather than accepting healthy status", func() {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		live := scopePolicy(map[string]string{"pci-15b3": "true"}, "source")
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		reads, validations := 0, 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, &workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				if reads == 1 {
					return apierrors.NewTooManyRequests("busy", 1)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		registry := crstate.NewRegistry()
		registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
			validations++
			return crstate.Result{State: crstate.StateSuccess}, nil
		})
		err := pollUntilTerminalWithRetryableErrorTimeout(ctx, c, registry, desired, "policy", "", 0, time.Millisecond, cfg)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		gomega.Expect(reads).To(gomega.Equal(2))
		gomega.Expect(validations).To(gomega.BeZero())
	})
	ginkgo.DescribeTable("bounds temporary scope retries by caller context", func(cancelDuringRead bool) {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		reads, validations := 0, 0
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			reads++
			if cancelDuringRead {
				cancel()
			}
			return apierrors.NewServiceUnavailable("temporarily unavailable")
		}}).Build()
		registry := crstate.NewRegistry()
		registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
			validations++
			return crstate.Result{State: crstate.StateSuccess}, nil
		})
		err := pollUntilTerminalWithRetryableErrorTimeout(pollCtx, c, registry, desired, "policy", "", 0, time.Millisecond, cfg)
		if cancelDuringRead {
			gomega.Expect(err).To(gomega.MatchError(context.Canceled))
			gomega.Expect(reads).To(gomega.Equal(1))
		} else {
			gomega.Expect(err).To(gomega.MatchError(context.DeadlineExceeded))
			gomega.Expect(reads).To(gomega.BeNumerically(">", 1))
		}
		gomega.Expect(validations).To(gomega.BeZero())
	},
		ginkgo.Entry("deadline", false),
		ginkgo.Entry("cancellation during read", true),
	)
	ginkgo.It("keeps original inventory intent after an admission-mutated initial apply response", func() {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		admitted := scopePolicy(map[string]string{"pci-15b3": "true"}, "preset")
		cfg.ClusterConfig = append(cfg.ClusterConfig, scopeGroup("preset"))
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		var live *unstructured.Unstructured
		applies := 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == desired.GroupVersionKind() && key.Name == desired.GetName() {
					if live == nil {
						return apierrors.NewNotFound(schema.GroupResource{Group: "sriovnetwork.openshift.io", Resource: "sriovnetworknodepolicies"}, key.Name)
					}
					obj.(*unstructured.Unstructured).Object = live.DeepCopy().Object
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applies++
				live = admitted.DeepCopy()
				// The real unstructured client decodes the API response into this wrapper.
				ac.(interface{ SetUnstructuredContent(map[string]interface{}) }).SetUnstructuredContent(live.DeepCopy().Object)
				return nil
			},
		}).Build()
		err := ApplyBundle(ctx, c, scopeBundle(desired, false), DeployOptions{Config: cfg, SkipHelmChart: true})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		gomega.Expect(applies).To(gomega.Equal(1))
		gomega.Expect(desired.GetAnnotations()[SourceGroupsAnnotation]).To(gomega.Equal(`["source"]`))
	})
	ginkgo.It("keeps original source and selector after a missing resource is reapplied", func() {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		original := desired.DeepCopy()
		admitted := scopePolicy(map[string]string{"pci-15b3": "true"}, "preset")
		cfg.ClusterConfig = append(cfg.ClusterConfig, scopeGroup("preset"))
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		var live *unstructured.Unstructured
		applies := 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == desired.GroupVersionKind() {
					if live == nil {
						return apierrors.NewNotFound(schema.GroupResource{Resource: "policies"}, key.Name)
					}
					obj.(*unstructured.Unstructured).Object = live.DeepCopy().Object
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applies++
				live = admitted.DeepCopy()
				ac.(interface{ SetUnstructuredContent(map[string]interface{}) }).SetUnstructuredContent(live.DeepCopy().Object)
				return nil
			},
		}).Build()
		registry := crstate.NewRegistry()
		registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
			return crstate.Result{State: crstate.StateNotDeployed}, nil
		})
		err := pollUntilTerminalWithRetryableErrorTimeout(ctx, c, registry, desired, "policy", "", 0, time.Millisecond, cfg)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		gomega.Expect(applies).To(gomega.Equal(1))
		gomega.Expect(desired.Object).To(gomega.Equal(original.Object))
	})
	ginkgo.It("blocks a dependency retry when labels drift while its API becomes available", func() {
		desired := scopePolicy(nil, "source")
		desired.SetAPIVersion("nv-ipam.nvidia.com/v1alpha1")
		desired.SetKind("IPPool")
		gomega.Expect(unstructured.SetNestedMap(desired.Object, map[string]interface{}{"nodeSelectorTerms": []interface{}{
			map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "cohort", "operator": "In", "values": []interface{}{"gpu"}}}},
		}}, "spec", "nodeSelector")).To(gomega.Succeed())
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		attempts, unsafeWrites := 0, 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&workers[0], &workers[1], &workers[2]).WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				attempts++
				if attempts == 1 {
					// The API appears after this failed request; another worker is now
					// accidentally labeled into the cohort before the next attempt.
					extra := &corev1.Node{}
					gomega.Expect(c.Get(ctx, client.ObjectKey{Name: "dpu-control"}, extra)).To(gomega.Succeed())
					extra.Labels["cohort"] = "gpu"
					gomega.Expect(c.Update(ctx, extra)).To(gomega.Succeed())
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "nv-ipam.nvidia.com", Kind: "IPPool"}}
				}
				unsafeWrites++
				return nil
			},
		}).Build()
		applied, err := retryDependentAPIWithInterval(ctx, c, desired, cfg, time.Millisecond)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("extra=[dpu-control]")))
		gomega.Expect(applied).To(gomega.BeNil())
		gomega.Expect(attempts).To(gomega.Equal(1))
		gomega.Expect(unsafeWrites).To(gomega.BeZero())
	})
	ginkgo.It("keeps selector-only scope visible alongside ordinary resource readiness", func() {
		pool := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "nv-ipam.nvidia.com/v1alpha1", "kind": "IPPool", "metadata": map[string]interface{}{"name": "pool"}}}
		c := fake.NewClientBuilder().WithObjects(pool).Build()
		results, err := ValidateBundleWithConfig(ctx, c, scopeBundle(pool, false), nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(results).To(gomega.HaveLen(1))
		gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateSuccess))
		gomega.Expect(results[0].Reason).To(gomega.ContainSubstring(selectorOnlyWarning))
	})
	ginkgo.It("does not convert denied node reads to verified scope", func() {
		desired := scopePolicy(map[string]string{"cohort": "gpu"}, "source")
		c := fake.NewClientBuilder().WithObjects(desired).WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return fmt.Errorf("forbidden nodes")
		}}).Build()
		err := checkBundleTargetScopes(ctx, c, scopeBundle(desired, false), cfg)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("forbidden nodes")))
		results, err := ValidateBundleWithConfig(ctx, c, scopeBundle(desired, false), cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateError))
	})
	ginkgo.It("adds no node reads to unrelated bundles", func() {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "other"}}}
		gomega.Expect(checkBundleTargetScopes(ctx, nil, scopeBundle(obj, false), cfg)).To(gomega.Succeed())
	})
})

// Extend the existing renderer regression suite with real profiles and bundle
// parsing, exercising full cohorts and strict source subsets without a cluster.
func TestTargetScopeProfileRendering(t *testing.T) {
	cases := []string{"host-device-rdma", "ipoib-rdma-shared", "macvlan-rdma-shared", "sriov-ethernet-rdma", "sriov-ib-rdma", "sriov-ethernet-rdma-ocp", "sriov-ib-rdma-ocp", "spectrum-x-ra2.1"}
	for _, profile := range cases {
		for _, subset := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/subset-%v", profile, subset), func(t *testing.T) {
				cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"), ctrllog.Log)
				require.NoError(t, err)
				cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
				if profile == "spectrum-x-ra2.1" {
					cfg.Profile.SpectrumX = &config.ProfileSpectrumX{Enable: true, SPCXVersion: "RA2.1", NumberOfPlanes: 1, MultiplaneMode: "none"}
				}
				if strings.Contains(profile, "ib") {
					cfg.Profile.Fabric = "infiniband"
				}
				if strings.Contains(profile, "ocp") {
					cfg.Flavor = config.FlavorOCP
				}
				cfg.ClusterConfig[0].WorkerNodes = []string{"gpu-a"}
				cfg.ClusterConfig[1].WorkerNodes = []string{"gpu-b"}
				selected := cfg.ClusterConfig
				if subset {
					selected = selected[:1]
				}
				plans, _ := planRender(cfg.ClusterConfig, selected)
				planSubnets, err := preallocatePlanSubnets(cfg, plans)
				require.NoError(t, err)
				templates := []string{"20-ippool.yaml", "40-sriovnetworknodepolicy.yaml"}
				if profile == "spectrum-x-ra2.1" {
					templates = []string{"50-sriovnetworknodepolicy.yaml"}
				}
				emitted := 0
				for _, template := range templates {
					path := filepath.Join("..", "..", "profiles", profile, template)
					if (strings.Contains(template, "sriovnetwork") && !strings.Contains(profile, "sriov")) && profile != "spectrum-x-ra2.1" {
						continue
					}
					if profile == "sriov-ib-rdma-ocp" && strings.Contains(template, "ippool") {
						continue
					}
					rendered, err := renderForScope(path, cfg, plans, planSubnets)
					require.NoError(t, err)
					files := make([]bundle.File, 0, len(rendered))
					for name, content := range rendered {
						files = append(files, bundle.File{Name: name, Content: content})
					}
					artifacts, err := bundle.FromFiles(files)
					require.NoError(t, err)
					for _, doc := range artifacts.Documents() {
						obj := doc.ObjectCopy()
						if !hasTargetScope(obj) {
							continue
						}
						emitted++
						scope, err := resolveTargetScope(obj, cfg)
						require.NoError(t, err)
						require.Len(t, scope.workers, len(selected))
						require.NotEmpty(t, obj.GetAnnotations()[SourceGroupsAnnotation])
					}
				}
				require.Positive(t, emitted)
			})
		}
	}
}

func TestTargetScopeRenderingRejectsAmbiguousSourceIDs(t *testing.T) {
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.ClusterConfig[1].Identifier = cfg.ClusterConfig[0].Identifier
	plans, _ := planRender(cfg.ClusterConfig, cfg.ClusterConfig)
	_, err = renderForScope(filepath.Join("..", "..", "profiles", "sriov-ethernet-rdma", "40-sriovnetworknodepolicy.yaml"), cfg, plans, nil)
	require.ErrorContains(t, err, "duplicate identifier")
}

func TestTargetScopeUnnamedSourceLifecycle(t *testing.T) {
	for _, input := range []string{"defaults", "canonical YAML"} {
		t.Run(input, func(t *testing.T) {
			var cfg *config.LaunchKitConfig
			var err error
			if input == "defaults" {
				cfg, err = config.DefaultLaunchKitConfig()
			} else {
				cfg, err = config.LoadFullConfig(filepath.Join("..", "..", "cluster-config.yaml"), ctrllog.Log)
			}
			require.NoError(t, err)
			require.Len(t, cfg.ClusterConfig, 1)
			require.Empty(t, cfg.ClusterConfig[0].Identifier)
			require.NoError(t, config.ValidateClusterConfigIdentifiers(cfg.ClusterConfig))
			cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
			source := &cfg.ClusterConfig[0]
			source.PFs = source.PFs[:1]
			source.WorkerNodes = []string{"gpu-a", "gpu-b"}
			source.NodeSelector = map[string]string{"cohort": "gpu"}
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
			require.NoError(t, err)
			files := make([]bundle.File, 0, len(rendered))
			for name, content := range rendered {
				files = append(files, bundle.File{Name: name, Content: content})
			}
			generated, err := bundle.FromFiles(files)
			require.NoError(t, err)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			objects := []client.Object{
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-a", Labels: map[string]string{"cohort": "gpu"}}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-b", Labels: map[string]string{"cohort": "gpu"}}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"cohort": "other"}}},
			}
			var targetFiles []bundle.File
			for _, doc := range generated.Documents() {
				obj := doc.ObjectCopy()
				if !hasTargetScope(obj) {
					continue
				}
				require.Equal(t, `[""]`, obj.GetAnnotations()[SourceGroupsAnnotation])
				scope, err := resolveTargetScope(obj, cfg)
				require.NoError(t, err)
				require.Equal(t, source.WorkerNodes, scope.workers)
				_, err = resolveTargetScope(obj, nil)
				require.Error(t, err, "losing sidecar must never downgrade annotated unnamed inventory to selector-only")
				data, err := yaml.Marshal(obj.Object)
				require.NoError(t, err)
				targetFiles = append(targetFiles, bundle.File{Name: fmt.Sprintf("%d-target.yaml", len(targetFiles)), Content: string(data)})
				objects = append(objects, obj)
				if obj.GetKind() == "SriovNetworkNodePolicy" {
					names, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "nicSelector", "pfNames")
					require.NoError(t, err)
					pfName := source.PFs[0].NetworkInterface
					if len(names) > 0 {
						pfName = strings.Split(names[0], "#")[0]
					}
					numVfs, _, err := unstructured.NestedInt64(obj.Object, "spec", "numVfs")
					require.NoError(t, err)
					for _, node := range source.WorkerNodes {
						objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{
							"apiVersion": "sriovnetwork.openshift.io/v1", "kind": "SriovNetworkNodeState",
							"metadata": map[string]interface{}{"name": node, "namespace": obj.GetNamespace()},
							"status":   map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"name": pfName, "pciAddress": source.PFs[0].PciAddress, "numVfs": numVfs}}},
						}})
					}
				}
			}
			require.NotEmpty(t, targetFiles)
			targets, err := bundle.FromFiles(targetFiles)
			require.NoError(t, err)
			applies := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					applies++
					return nil
				},
			}).Build()
			require.NoError(t, ApplyBundle(context.Background(), c, targets, DeployOptions{Config: cfg, SkipHelmChart: true}))
			require.Equal(t, len(targetFiles), applies)
			results, err := ValidateBundleWithConfig(context.Background(), c, targets, cfg)
			require.NoError(t, err)
			for _, result := range results {
				require.Equal(t, crstate.StateSuccess, result.State)
				require.NotContains(t, result.Reason, selectorOnlyWarning)
			}
			extra := &corev1.Node{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "other"}, extra))
			extra.Labels["cohort"] = "gpu"
			require.NoError(t, c.Update(context.Background(), extra))
			require.ErrorContains(t, ApplyBundle(context.Background(), c, targets, DeployOptions{Config: cfg, SkipHelmChart: true}), "extra=[other]")
			require.Equal(t, len(targetFiles), applies, "drift must block before another apply")
			results, err = ValidateBundleWithConfig(context.Background(), c, targets, cfg)
			require.NoError(t, err)
			for _, result := range results {
				require.Equal(t, crstate.StateError, result.State)
				require.Contains(t, result.Reason, "extra=[other]")
			}
		})
	}
}
