// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// No test in this suite may use a live HTTP endpoint, including Helm reads.
type policyTestTransport struct{}

func (policyTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("HTTP disabled in deployment policy tests")
}

func policyTestConfig() *config.LaunchKitConfig {
	// Deliberately differs from the explicit OCP selection in those cases.
	return &config.LaunchKitConfig{Flavor: config.FlavorK8s, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
}

func policyTestOperators() []client.Object {
	var objects []client.Object
	for _, op := range []struct{ name, namespace, api string }{
		{"nvidia-network-operator", "operator-ns", "nicclusterpolicies.mellanox.com"},
		{"nfd", config.DefaultNFDOperatorNamespace, "nodefeaturediscoveries.nfd.openshift.io"},
		{"nvidia-maintenance-operator", config.DefaultMaintenanceOperatorNamespace, "maintenanceoperatorconfigs.maintenance.nvidia.com"},
	} {
		csvName := op.name + ".v26.7.0"
		objects = append(objects,
			ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", op.namespace, op.name, map[string]any{
				"spec": map[string]any{"name": op.name}, "status": map[string]any{"installedCSV": csvName},
			}),
			ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", op.namespace, csvName, map[string]any{
				"status": map[string]any{"phase": "Succeeded"},
				"spec":   map[string]any{"version": "26.7.0", "install": map[string]any{"spec": map[string]any{"deployments": []any{map[string]any{"name": "network-operator"}}}}},
			}),
			ocpTestObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", op.api, map[string]any{
				"spec":   map[string]any{"versions": []any{map[string]any{"name": "v1alpha1", "served": true}}},
				"status": map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
			}),
		)
	}
	objects = append(objects, ocpTestObject("apps/v1", "Deployment", "operator-ns", "network-operator", map[string]any{
		"spec": map[string]any{"replicas": int64(1), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
			"name": "operator", "env": []any{
				map[string]any{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "true"},
				map[string]any{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": config.DefaultMaintenanceOperatorNamespace},
			},
		}}}}},
		"status": map[string]any{"replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1)},
	}))
	return objects
}

func policyTestBundle(platform hostplatform.Selection, withValues bool, extra ...bundle.File) *bundle.Bundle {
	files := []bundle.File{
		{Name: "10-ncp.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n"},
		{Name: "20-nnp.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicNodePolicy\nmetadata:\n  name: node-policy\n"},
		{Name: "40-extra.yaml", Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: operator-ns\n"},
	}
	if platform == hostplatform.OpenShift {
		files = append(files, bundle.File{Name: "05-config.yaml", Content: "apiVersion: maintenance.nvidia.com/v1alpha1\nkind: MaintenanceOperatorConfig\nmetadata:\n  name: default\n  namespace: maintenance-ns\nspec:\n  maxParallelOperations: 1\n"})
	}
	if withValues {
		files = append(files, bundle.File{Name: "values.yaml", Content: "operator: {}\n"})
	}
	artifacts, err := bundle.FromFiles(append(files, extra...))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return artifacts
}

var _ = ginkgo.Describe("concrete Host policies through the deployment coordinator", func() {
	for _, mode := range []struct {
		name     string
		platform hostplatform.Selection
		external bool
	}{
		{"Kubernetes managed Helm", hostplatform.Kubernetes, false},
		{"Kubernetes external Helm", hostplatform.Kubernetes, true},
		{"OpenShift OLM", hostplatform.OpenShift, false},
	} {
		ginkgo.It("preserves bootstrap, preflight, configuration and fresh observation order for "+mode.name, func() {
			cfg := policyTestConfig()
			artifacts := policyTestBundle(mode.platform, mode.platform == hostplatform.Kubernetes)
			before := artifacts.Documents()[0].ObjectCopy()
			applied := map[string]bool{}
			observed := map[string]bool{}
			prerequisitesRead, preflightScanned, configured, helmInstalled := false, false, false, false
			installCalls := 0
			objects := []client.Object{}
			if mode.platform == hostplatform.OpenShift {
				objects = policyTestOperators()
			}
			scheme := runtime.NewScheme()
			gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					kind := obj.GetObjectKind().GroupVersionKind().Kind
					if kind == "CustomResourceDefinition" && key.Name == "maintenanceoperatorconfigs.maintenance.nvidia.com" {
						prerequisitesRead = true
					}
					if (kind == "NicClusterPolicy" || kind == "NicNodePolicy") && applied[kind] {
						live := obj.(*unstructured.Unstructured)
						live.SetName(key.Name)
						live.SetResourceVersion("2")
						gomega.Expect(unstructured.SetNestedField(live.Object, "ready", "status", "state")).To(gomega.Succeed())
						observed[kind] = true
						return nil
					}
					if kind == "ConfigMap" && key.Name == "extra" && applied[kind] {
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if list.GetObjectKind().GroupVersionKind().Kind == "NicNodePolicyList" {
						preflightScanned = true
						if mode.platform == hostplatform.OpenShift {
							gomega.Expect(prerequisitesRead).To(gomega.BeTrue())
						} else if !mode.external {
							gomega.Expect(helmInstalled).To(gomega.BeTrue())
						}
					}
					return c.List(ctx, list, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					gomega.Expect(preflightScanned).To(gomega.BeTrue())
					gomega.Expect(applied).To(gomega.BeEmpty())
					configured = true
					return c.Create(ctx, obj, opts...)
				},
				Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					obj := ac.(client.Object)
					kind := obj.GetObjectKind().GroupVersionKind().Kind
					gomega.Expect(preflightScanned).To(gomega.BeTrue())
					if mode.platform == hostplatform.OpenShift {
						gomega.Expect(configured).To(gomega.BeTrue())
					}
					if kind == "NicNodePolicy" {
						gomega.Expect(observed["NicClusterPolicy"]).To(gomega.BeTrue())
					}
					if kind == "ConfigMap" {
						gomega.Expect(observed["NicNodePolicy"]).To(gomega.BeTrue())
					}
					applied[kind] = true
					obj.SetGeneration(1)
					obj.SetResourceVersion("1")
					return nil
				},
			}).Build()
			opts := DeployOptions{Platform: mode.platform, Config: cfg, SkipHelmChart: mode.external,
				NetworkOperator: cfg.NetworkOperator,
				RestConfig:      &rest.Config{Host: "https://example.invalid", Transport: policyTestTransport{}},
			}
			opts.NetworkOperator.HelmRepoURL = "https://example.invalid"
			opts.NetworkOperator.Version = "v1"
			install := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
				installCalls++
				gomega.Expect(applied).To(gomega.BeEmpty())
				gomega.Expect(preflightScanned).To(gomega.BeFalse())
				helmInstalled = true
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			gomega.Expect(applyBundleWithInstaller(ctx, c, artifacts, opts, install)).To(gomega.Succeed())
			gomega.Expect(applied).To(gomega.HaveLen(3))
			gomega.Expect(observed).To(gomega.HaveLen(2))
			if mode.platform == hostplatform.Kubernetes && !mode.external {
				gomega.Expect(installCalls).To(gomega.Equal(1))
			} else {
				gomega.Expect(installCalls).To(gomega.BeZero())
			}
			gomega.Expect(artifacts.Documents()[0].ObjectCopy()).To(gomega.Equal(before))
		})
	}

	ginkgo.It("stops managed Helm failure before scans or CR application", func() {
		failed := errors.New("installer failed")
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				ginkgo.Fail("preflight must not run after bootstrap failure")
				return nil
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				ginkgo.Fail("CR apply must not run after bootstrap failure")
				return nil
			},
		}).Build()
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}\n"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = applyBundleWithInstaller(context.Background(), c, artifacts, DeployOptions{Platform: hostplatform.Kubernetes,
			RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{Version: "v1", HelmRepoURL: "https://example.invalid"},
		}, func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
			return failed
		})
		gomega.Expect(errors.Is(err, failed)).To(gomega.BeTrue())
	})

	ginkgo.It("refuses missing OLM prerequisites before configuration or apply", func() {
		writes := 0
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				writes++
				return nil
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				writes++
				return nil
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				writes++
				return nil
			},
		}).Build()
		err := ApplyBundle(context.Background(), c, policyTestBundle(hostplatform.OpenShift, false), DeployOptions{Platform: hostplatform.OpenShift, Config: policyTestConfig()})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("not installed")))
		gomega.Expect(writes).To(gomega.BeZero())
	})

	ginkgo.It("checks the allowlist before every other operator or policy write", func() {
		writes := 0
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policyTestOperators()...).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				writes++
				return nil
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				writes++
				return nil
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				writes++
				return nil
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				writes++
				return nil
			},
		}).Build()
		// Sort after the other operator config to prove the prerequisite does
		// not depend on artifact order.
		patch := bundle.File{Name: "99-patch.yaml", Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cni-sysctl-allowlist\n  namespace: openshift-multus\ndata:\n  allowlist.conf: '^net.ipv4.conf.IFNAME.arp_ignore$'\n"}
		err := ApplyBundle(context.Background(), c, policyTestBundle(hostplatform.OpenShift, false, patch), DeployOptions{Platform: hostplatform.OpenShift, Config: policyTestConfig()})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("cni-sysctl-allowlist")))
		gomega.Expect(writes).To(gomega.BeZero())
	})

	for _, platform := range []hostplatform.Selection{hostplatform.Kubernetes, hostplatform.OpenShift} {
		ginkgo.It("scans strays before applying platform preflight policy for "+string(platform), func() {
			scanned, writes := false, 0
			objects := []client.Object{ocpTestObject("mellanox.com/v1alpha1", "NicNodePolicy", "", "stray", nil)}
			if platform == hostplatform.OpenShift {
				objects = append(objects, policyTestOperators()...)
			}
			c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if list.GetObjectKind().GroupVersionKind().Kind == "NicNodePolicyList" {
						scanned = true
					}
					return c.List(ctx, list, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					writes++
					return errors.New("reached configuration")
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					writes++
					return errors.New("reached application")
				},
			}).Build()
			err := ApplyBundle(context.Background(), c, policyTestBundle(platform, false), DeployOptions{Platform: platform, Config: policyTestConfig(), NetworkOperator: policyTestConfig().NetworkOperator})
			gomega.Expect(scanned).To(gomega.BeTrue())
			if platform == hostplatform.OpenShift {
				gomega.Expect(err).To(gomega.MatchError("configure MaintenanceOperatorConfig maintenance-ns/default: reached configuration"))
				gomega.Expect(writes).To(gomega.Equal(1))
			} else {
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("preflight found 1 issue")))
				gomega.Expect(writes).To(gomega.BeZero())
			}
		})
	}

	for _, platform := range []hostplatform.Selection{hostplatform.Kubernetes, hostplatform.OpenShift} {
		ginkgo.It("rejects stale bootstrap readiness before later applies for "+string(platform), func() {
			objects := []client.Object{}
			if platform == hostplatform.OpenShift {
				objects = policyTestOperators()
			}
			applies, staleReads := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == "NicClusterPolicy" && applies > 0 {
						live := obj.(*unstructured.Unstructured)
						live.SetName(key.Name)
						live.SetResourceVersion("apply-rv")
						gomega.Expect(unstructured.SetNestedField(live.Object, "ready", "status", "state")).To(gomega.Succeed())
						staleReads++
						cancel()
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					obj := ac.(client.Object)
					gomega.Expect(obj.GetObjectKind().GroupVersionKind().Kind).To(gomega.Equal("NicClusterPolicy"))
					applies++
					obj.SetGeneration(1)
					obj.SetResourceVersion("apply-rv")
					return nil
				},
			}).Build()
			err := ApplyBundle(ctx, c, policyTestBundle(platform, false), DeployOptions{Platform: platform, Config: policyTestConfig()})
			gomega.Expect(errors.Is(err, context.Canceled)).To(gomega.BeTrue())
			gomega.Expect(staleReads).To(gomega.Equal(1))
			gomega.Expect(applies).To(gomega.Equal(1))
		})
	}

	ginkgo.It("retains component version failures before OCP configuration writes", func() {
		stray := ocpTestObject("mellanox.com/v1alpha1", "NicClusterPolicy", "", "policy", map[string]any{
			"spec": map[string]any{"nvIpam": map[string]any{"version": "old"}},
		})
		objects := append(policyTestOperators(), stray)
		writes := 0
		c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				writes++
				return nil
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				writes++
				return nil
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				writes++
				return nil
			},
		}).Build()
		cfg := policyTestConfig()
		cfg.NetworkOperator.ComponentVersion = "new"
		err := ApplyBundle(context.Background(), c, policyTestBundle(hostplatform.OpenShift, false), DeployOptions{Platform: hostplatform.OpenShift, Config: cfg, NetworkOperator: cfg.NetworkOperator})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("component versions")))
		gomega.Expect(writes).To(gomega.BeZero())
	})

	ginkgo.It("retries a missing OCP IPPool API only after freshly observed policies", func() {
		cfg := policyTestConfig()
		cfg.Cluster = &config.ClusterNetworkConfig{PodCIDRs: []string{"10.0.0.0/16"}, ServiceCIDRs: []string{"10.1.0.0/16"}}
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		node := &corev1.Node{}
		node.Name = "worker-a"
		objects := append(policyTestOperators(), node)
		applied := map[string]bool{}
		observed := map[string]bool{}
		poolsApplied := 0
		reachedExtra := errors.New("reached extra after dependent API retry")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				kind := obj.GetObjectKind().GroupVersionKind().Kind
				if (kind == "NicClusterPolicy" || kind == "NicNodePolicy") && applied[kind] {
					live := obj.(*unstructured.Unstructured)
					live.SetName(key.Name)
					live.SetResourceVersion("observed-rv")
					gomega.Expect(unstructured.SetNestedField(live.Object, "ready", "status", "state")).To(gomega.Succeed())
					observed[kind] = true
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				gomega.Expect((&client.ApplyOptions{}).ApplyOptions(opts).DryRun).To(gomega.BeEmpty())
				obj := ac.(client.Object)
				kind := obj.GetObjectKind().GroupVersionKind().Kind
				if kind == "IPPool" {
					gomega.Expect(observed).To(gomega.HaveLen(2))
					poolsApplied++
					if poolsApplied == 1 {
						// Simulate a server response mutating the submitted copy;
						// retry must still use the original desired object.
						obj.SetLabels(map[string]string{"server-response": "discard"})
						return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "nv-ipam.nvidia.com", Kind: "IPPool"}}
					}
					gomega.Expect(obj.GetLabels()).NotTo(gomega.HaveKey("server-response"))
					return nil
				}
				if kind == "ConfigMap" {
					return reachedExtra
				}
				applied[kind] = true
				obj.SetGeneration(1)
				obj.SetResourceVersion("apply-rv")
				return nil
			},
		}).Build()
		pool := bundle.File{Name: "30-pool.yaml", Content: "apiVersion: nv-ipam.nvidia.com/v1alpha1\nkind: IPPool\nmetadata:\n  name: pool\n  namespace: operator-ns\nspec:\n  subnet: 192.0.2.0/24\n"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := ApplyBundle(ctx, c, policyTestBundle(hostplatform.OpenShift, false, pool), DeployOptions{Platform: hostplatform.OpenShift, Config: cfg})
		gomega.Expect(errors.Is(err, reachedExtra)).To(gomega.BeTrue())
		gomega.Expect(poolsApplied).To(gomega.Equal(2))
	})

	ginkgo.It("keeps deferred OCP IPPool dry-run validation as an error after bootstrap", func() {
		cfg := policyTestConfig()
		cfg.Cluster = &config.ClusterNetworkConfig{PodCIDRs: []string{"10.0.0.0/16"}, ServiceCIDRs: []string{"10.1.0.0/16"}}
		scheme := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
		node := &corev1.Node{}
		node.Name = "worker-a"
		objects := append(policyTestOperators(), node)
		policiesApplied, poolsApplied := 0, 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				gomega.Expect((&client.ApplyOptions{}).ApplyOptions(opts).DryRun).To(gomega.Equal([]string{"All"}))
				kind := ac.(client.Object).GetObjectKind().GroupVersionKind().Kind
				if kind == "IPPool" {
					poolsApplied++
					gomega.Expect(policiesApplied).To(gomega.Equal(2))
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "nv-ipam.nvidia.com", Kind: "IPPool"}}
				}
				if kind == "NicClusterPolicy" || kind == "NicNodePolicy" {
					policiesApplied++
				}
				return nil
			},
		}).Build()
		pool := bundle.File{Name: "30-pool.yaml", Content: "apiVersion: nv-ipam.nvidia.com/v1alpha1\nkind: IPPool\nmetadata:\n  name: pool\n  namespace: operator-ns\nspec:\n  subnet: 192.0.2.0/24\n"}
		err := ApplyBundle(context.Background(), c, policyTestBundle(hostplatform.OpenShift, false, pool), DeployOptions{Platform: hostplatform.OpenShift, Config: cfg, DryRun: true})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("dry-run deferred validation of")))
		gomega.Expect(poolsApplied).To(gomega.Equal(1))
	})
})
