// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
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

const sysctlPatchFilename = "04-cni-sysctl-allowlist-patch.yaml"

func sysctlPatch(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	rendered, err := ProcessTemplate(filepath.Join("..", "..", "profiles", "sriov-ethernet-rdma-ocp", sysctlPatchFilename), &config.LaunchKitConfig{Profile: &config.Profile{IgnoreARP: true}}, "")
	require.NoError(t, err)
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: sysctlPatchFilename, Content: rendered[sysctlPatchFilename]}})
	require.NoError(t, err)
	require.Len(t, artifacts.Documents(), 1)
	return artifacts.Documents()[0].ObjectCopy()
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
	desired := sysctlPatch(t)
	require.NoError(t, ensureOCPSysctlAllowlist(ctx, c, desired, false))
	require.Equal(t, 2, attempts)
	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(initial), got))
	require.Equal(t, initial.Labels, got.Labels)
	require.Equal(t, "keep", got.Data["unrelated"])
	require.True(t, strings.HasPrefix(got.Data["allowlist.conf"], initial.Data["allowlist.conf"]))
	require.Contains(t, got.Data["allowlist.conf"], "^net.ipv4.conf.IFNAME.concurrent$")
	required, _, err := unstructured.NestedString(desired.Object, "data", "allowlist.conf")
	require.NoError(t, err)
	for _, line := range strings.Split(required, "\n") {
		require.Equal(t, 1, strings.Count(got.Data["allowlist.conf"], line))
	}
	require.NoError(t, ensureOCPSysctlAllowlist(ctx, c, desired, false))
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
			err := ensureOCPSysctlAllowlist(context.Background(), c, sysctlPatch(t), mode == "dry-run")
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
					patchContent, hasPatch := rendered[sysctlPatchFilename]
					require.Equal(t, enabled && flavor == config.FlavorOCP, hasPatch)
					if hasPatch {
						artifacts, err := bundle.FromFiles([]bundle.File{{Name: sysctlPatchFilename, Content: patchContent}})
						require.NoError(t, err)
						selected, err := prepareDeploymentObjects(artifacts, true)
						require.NoError(t, err)
						require.Len(t, selected.operatorConfigs, 1)
						require.Empty(t, selected.others, "patch must not enter ordinary resource apply")
						require.True(t, isOCPSysctlAllowlistPatch(selected.operatorConfigs[0]))
						entries, _, err := unstructured.NestedString(selected.operatorConfigs[0].Object, "data", "allowlist.conf")
						require.NoError(t, err)
						require.Len(t, strings.Split(strings.TrimSpace(entries), "\n"), 6)
						for _, scope := range []string{"all", "IFNAME"} {
							for _, key := range []string{"arp_ignore", "arp_announce", "rp_filter"} {
								require.Contains(t, entries, "^net.ipv4.conf."+scope+"."+key+"$")
							}
						}
					}
					names := fileNamesMatching(rendered, profile.networkFile)
					require.NotEmpty(t, names)
					for _, name := range names {
						for _, doc := range parseDocs(t, name, rendered[name]) {
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

func TestSysctlPatchArtifactRoundTripAndEdits(t *testing.T) {
	desired := sysctlPatch(t)
	custom := "^net.ipv4.conf.all.arp_ignore$\n^net.ipv4.conf.IFNAME.site_custom$"
	require.NoError(t, unstructured.SetNestedField(desired.Object, custom, "data", "allowlist.conf"))
	data, err := desired.MarshalJSON()
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, sysctlPatchFilename), data, 0600))
	artifacts, err := bundle.Load(os.DirFS(dir))
	require.NoError(t, err)
	_, err = prepareDeploymentObjects(artifacts, false)
	require.ErrorContains(t, err, "requires flavor ocp")
	selected, err := prepareDeploymentObjects(artifacts, true)
	require.NoError(t, err)
	require.Len(t, selected.operatorConfigs, 1)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	initial := sysctlConfigMap()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initial).Build()
	require.NoError(t, ensureOCPSysctlAllowlist(context.Background(), c, selected.operatorConfigs[0], false))
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(desired.GroupVersionKind())
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(initial), current))
	got, _, err := unstructured.NestedString(current.Object, "data", "allowlist.conf")
	require.NoError(t, err)
	require.Contains(t, got, "site_custom")
	require.NotContains(t, got, "arp_announce", "deploy must use the artifact, not a hidden built-in list")
	results, err := ValidateBundle(context.Background(), c, artifacts, config.FlavorOCP)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, crstate.StateSuccess, results[0].State)
	live := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(initial), live))
	live.Data["allowlist.conf"] = ""
	require.NoError(t, c.Update(context.Background(), live))
	results, err = ValidateBundle(context.Background(), c, artifacts, config.FlavorOCP)
	require.NoError(t, err)
	require.Equal(t, crstate.StateError, results[0].State)

	before := current.DeepCopy()
	require.NoError(t, mergeOCPConfiguration(current, selected.operatorConfigs[0]))
	require.Equal(t, before.Object, current.Object, "validation accepts extra live entries")
	require.NoError(t, unstructured.SetNestedField(current.Object, "", "data", "allowlist.conf"))
	before = current.DeepCopy()
	require.NoError(t, mergeOCPConfiguration(current, selected.operatorConfigs[0]))
	require.NotEqual(t, before.Object, current.Object, "validation detects missing entries")
}

func TestSysctlPatchRejectsInvalidArtifacts(t *testing.T) {
	for _, mode := range []string{"wrong target", "missing entries", "non-string entries"} {
		t.Run(mode, func(t *testing.T) {
			desired := sysctlPatch(t)
			switch mode {
			case "wrong target":
				desired.SetNamespace("other")
			case "missing entries":
				unstructured.RemoveNestedField(desired.Object, "data", "allowlist.conf")
			case "non-string entries":
				require.NoError(t, unstructured.SetNestedField(desired.Object, int64(1), "data", "allowlist.conf"))
			}
			data, err := desired.MarshalJSON()
			require.NoError(t, err)
			artifacts, err := bundle.FromFiles([]bundle.File{{Name: sysctlPatchFilename, Content: string(data)}})
			require.NoError(t, err)
			_, err = prepareDeploymentObjects(artifacts, true)
			require.ErrorContains(t, err, sysctlPatchFilename)
		})
	}
}

func TestOCPOperatorConfigurationConsumesSysctlPatch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-ns", "network-operator", map[string]interface{}{
		"spec": map[string]interface{}{"name": "nvidia-network-operator"},
	})
	patches := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sub, sysctlConfigMap()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			require.Equal(t, "cni-sysctl-allowlist", obj.GetName())
			options := &client.PatchOptions{}
			options.ApplyOptions(opts)
			require.Equal(t, []string{metav1.DryRunAll}, options.DryRun)
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	require.NoError(t, applyOCPOperatorConfiguration(context.Background(), c, cfg, []*unstructured.Unstructured{sysctlPatch(t)}, true))
	require.Equal(t, 1, patches)
}
