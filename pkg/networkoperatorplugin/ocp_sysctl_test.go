// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func arpNetwork() *unstructured.Unstructured {
	return ocpTestObject("sriovnetwork.openshift.io/v1", "SriovNetwork", "sriov-ns", "rdma", map[string]interface{}{
		"spec": map[string]interface{}{"metaPlugins": secondaryNetworkMetaPlugins(&config.Profile{IgnoreARP: true})},
	})
}

func sysctlConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cni-sysctl-allowlist", Namespace: "openshift-multus", Labels: map[string]string{"owner": "platform"}}, Data: map[string]string{
		"allowlist.conf": "# site policy\n^net.ipv4.conf.IFNAME.arp_ignore$\n^net.ipv4.conf.IFNAME.custom$", "unrelated": "keep",
	}}
}

func TestOCPSysctlAllowlistPreservesAndRetries(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	initial := sysctlConfigMap()
	attempts := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			attempts++
			data, err := patch.Data(obj)
			require.NoError(t, err)
			require.Contains(t, string(data), "resourceVersion", "concurrent edits must be protected")
			if attempts == 1 {
				current := &corev1.ConfigMap{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(initial), current))
				current.Data["allowlist.conf"] += "\n^net.ipv4.conf.IFNAME.concurrent$"
				require.NoError(t, c.Update(ctx, current))
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, initial.Name, errors.New("concurrent edit"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	docs := []*unstructured.Unstructured{arpNetwork()}
	require.NoError(t, ensureOCPSysctlAllowlist(ctx, c, docs, false))
	require.Equal(t, 2, attempts)
	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(initial), got))
	require.Equal(t, initial.Labels, got.Labels)
	require.Equal(t, "keep", got.Data["unrelated"])
	require.True(t, strings.HasPrefix(got.Data["allowlist.conf"], initial.Data["allowlist.conf"]))
	require.Contains(t, got.Data["allowlist.conf"], "^net.ipv4.conf.IFNAME.concurrent$")
	for _, line := range strings.Split(ocpIgnoreARPAllowlist, "\n") {
		require.Equal(t, 1, strings.Count(got.Data["allowlist.conf"], line))
	}
	require.NoError(t, ensureOCPSysctlAllowlist(ctx, c, docs, false))
	require.Equal(t, 2, attempts, "second deploy should not patch")
}

func TestOCPSysctlAllowlistDryRunAndFailures(t *testing.T) {
	for _, mode := range []string{"dry-run", "missing", "get denied", "patch denied", "empty data"} {
		t.Run(mode, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			initial := sysctlConfigMap()
			if mode == "empty data" {
				initial.Data = nil
			}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if mode != "missing" {
				builder = builder.WithObjects(initial)
			}
			patches := 0
			c := builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if mode == "get denied" {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, errors.New("denied"))
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if mode == "patch denied" {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("denied"))
					}
					if mode == "dry-run" {
						options := &client.PatchOptions{}
						options.ApplyOptions(opts)
						require.Equal(t, []string{metav1.DryRunAll}, options.DryRun)
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			err := ensureOCPSysctlAllowlist(context.Background(), c, []*unstructured.Unstructured{arpNetwork()}, mode == "dry-run")
			switch mode {
			case "missing", "get denied", "patch denied":
				require.ErrorContains(t, err, "openshift-multus/cni-sysctl-allowlist")
				if mode != "patch denied" {
					require.Zero(t, patches)
				}
			default:
				require.NoError(t, err)
				require.Equal(t, 1, patches)
				got := &corev1.ConfigMap{}
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(initial), got))
				if mode == "dry-run" {
					require.Equal(t, initial.Data, got.Data)
				} else {
					require.Contains(t, got.Data["allowlist.conf"], "^net.ipv4.conf.all.arp_ignore$")
				}
			}
		})
	}
}

func TestOCPSysctlAllowlistUsesDesiredNetworks(t *testing.T) {
	for _, tc := range []struct {
		name, kind, group, meta string
		want, wantErr           bool
	}{
		{"enabled", "SriovNetwork", "sriovnetwork.openshift.io/v1", secondaryNetworkMetaPlugins(&config.Profile{IgnoreARP: true}), true, false},
		{"disabled", "SriovNetwork", "sriovnetwork.openshift.io/v1", "", false, false},
		{"routing only", "SriovNetwork", "sriovnetwork.openshift.io/v1", `{"type":"sbr"}`, false, false},
		{"spectrum x", "SriovNetwork", "sriovnetwork.openshift.io/v1", secondaryNetworkMetaPlugins(&config.Profile{IgnoreARP: true, SpectrumX: &config.ProfileSpectrumX{Enable: true}}), false, false},
		{"unrelated object", "ConfigMap", "v1", `invalid`, false, false},
		{"invalid meta", "SriovNetwork", "sriovnetwork.openshift.io/v1", `invalid`, false, true},
		{"legacy IFNAME only", "MacvlanNetwork", "mellanox.com/v1alpha1", `{"type":"tuning","sysctl":{"net.ipv4.conf.IFNAME.arp_ignore":"1"}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := ocpTestObject(tc.group, tc.kind, "network-ns", "test", map[string]interface{}{"spec": map[string]interface{}{"metaPlugins": tc.meta}})
			got, err := networksNeedARPAllowlist([]*unstructured.Unstructured{obj})
			require.Equal(t, tc.wantErr, err != nil)
			require.Equal(t, tc.want, got)
			if !tc.want && !tc.wantErr {
				// A nil client proves disabled cases don't even inspect cluster state.
				require.NoError(t, ensureOCPSysctlAllowlist(context.Background(), nil, []*unstructured.Unstructured{obj}, false))
			}
		})
	}
}

func TestIgnoreARPMatchesAcrossFlavors(t *testing.T) {
	for _, profile := range []struct{ name, fabric, deployment, networkFile string }{
		{"sriov-ethernet-rdma", "ethernet", "sriov", "50-sriovnetwork"},
		{"sriov-ib-rdma", "infiniband", "sriov", "50-sriovibnetwork"},
		{"host-device-rdma", "ethernet", "host_device", "30-hostdevicenetwork"},
		{"macvlan-rdma-shared", "ethernet", "rdma_shared", "30-macvlannetwork"},
		{"ipoib-rdma-shared", "infiniband", "rdma_shared", "30-ipoibnetwork"},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ignoreARP=%t", profile.name, enabled), func(t *testing.T) {
				var previous string
				for _, flavor := range []string{config.FlavorK8s, config.FlavorOCP} {
					cfg := standardNicConfigurationConfig(t, flavor, profile.fabric, profile.deployment, false)
					cfg.Profile.IgnoreARP = enabled
					cfg.Profile.Routing = config.RoutingSourceBased
					dir := profile.name
					if flavor == config.FlavorOCP {
						dir += "-ocp"
					}
					rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, dir), cfg)
					require.NoError(t, err)
					names := fileNamesMatching(rendered, profile.networkFile)
					require.NotEmpty(t, names)
					for _, name := range names {
						for _, doc := range parseDocs(t, name, rendered[name]) {
							needed, err := networksNeedARPAllowlist([]*unstructured.Unstructured{{Object: doc}})
							require.NoError(t, err)
							require.Equal(t, enabled, needed, "deploy must recognize rendered tuning for every network kind")
							meta := specString(t, doc, "metaPlugins")
							if previous != "" {
								require.Equal(t, previous, meta)
							}
							previous = meta
							var plugins []struct {
								Type   string            `json:"type"`
								Sysctl map[string]string `json:"sysctl"`
							}
							require.NoError(t, json.Unmarshal([]byte("["+meta+"]"), &plugins))
							if enabled {
								require.Len(t, plugins, 2)
								require.Equal(t, "tuning", plugins[0].Type)
								require.Equal(t, map[string]string{
									"net.ipv4.conf.all.arp_ignore": "1", "net.ipv4.conf.all.arp_announce": "2", "net.ipv4.conf.all.rp_filter": "0",
									"net.ipv4.conf.IFNAME.arp_ignore": "1", "net.ipv4.conf.IFNAME.arp_announce": "2", "net.ipv4.conf.IFNAME.rp_filter": "0",
								}, plugins[0].Sysctl)
							} else {
								require.Len(t, plugins, 1)
							}
							require.Equal(t, "sbr", plugins[len(plugins)-1].Type)
						}
					}
				}
			})
		}
	}
}
