// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const cleanupNFDRules = "sources: {pci: {deviceClassWhitelist: ['02', '03', '0280'], deviceLabelFields: [vendor]}}"

func cleanupNFDObject(rules string) *unstructured.Unstructured {
	obj := rollbackObject("nfd.openshift.io/v1", "NodeFeatureDiscovery", map[string]interface{}{"workerConfig": map[string]interface{}{"configData": rules}})
	obj.SetAnnotations(map[string]string{"site": "keep"})
	return obj
}

func TestOCPNFDContentCleanupPreservesOtherRulesAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name, live, remaining string
		changed               bool
	}{
		{"other PCI classes", "sources: {pci: {deviceClassWhitelist: ['02', '03', '0280', '0302'], deviceLabelFields: [vendor]}}", "sources: {pci: {deviceClassWhitelist: ['0302'], deviceLabelFields: [vendor]}}", true},
		{"other source", cleanupNFDRules + "\ncore: {sleepInterval: 2m}\nsources: {pci: {deviceClassWhitelist: ['02','03','0280'], deviceLabelFields: [vendor]}, custom: {labels: {site: keep}}}", "core: {sleepInterval: 2m}\nsources: {custom: {labels: {site: keep}}}", true},
		{"changed common format", "sources: {pci: {deviceClassWhitelist: ['02','03','0280'], deviceLabelFields: [class,vendor]}}", "sources: {pci: {deviceLabelFields: [class,vendor]}}", true},
		{"list order is significant", "sources: {pci: {deviceClassWhitelist: ['0280','03','02'], deviceLabelFields: [vendor]}}", "{}", true},
		{"site empty list", "# keep byte-for-byte\nsources: {pci: {deviceClassWhitelist: [], deviceLabelFields: [class,vendor]}}", "sources: {pci: {deviceClassWhitelist: [], deviceLabelFields: [class,vendor]}}", false},
		{"site empty stanza", "# keep byte-for-byte\nsources: {pci: {}}", "sources: {pci: {}}", false},
		{"implicit classes share format", "# keep byte-for-byte\nsources: {pci: {deviceLabelFields: [vendor]}, custom: {labels: {site: keep}}}", "sources: {pci: {deviceLabelFields: [vendor]}, custom: {labels: {site: keep}}}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			desired, initial := cleanupNFDObject(cleanupNFDRules), cleanupNFDObject(tc.live)
			patches := 0
			c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("NFD rule comparison must not inventory other resource kinds")
					return nil
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					t.Fatal("additional or different rules must retain the NFD")
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			for attempt := 0; attempt < 2; attempt++ {
				messages, err := cleanOCPNFDConfiguration(ctx, c, desired, initial)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				if attempt > 0 || !tc.changed {
					require.NotContains(t, messages[0], "removed")
				}
			}
			got := getRollbackObject(t, c, initial)
			expected, err := ocpConfigurationView(cleanupNFDObject(tc.remaining))
			require.NoError(t, err)
			actual, err := ocpConfigurationView(got)
			require.NoError(t, err)
			require.Equal(t, expected, actual)
			require.Equal(t, initial.GetUID(), got.GetUID())
			require.Equal(t, initial.GetAnnotations(), got.GetAnnotations())
			if tc.changed {
				require.Equal(t, 1, patches)
			} else {
				require.Zero(t, patches)
				raw, _, err := unstructured.NestedString(got.Object, "spec", "workerConfig", "configData")
				require.NoError(t, err)
				require.Equal(t, tc.live, raw)
			}
		})
	}
}

func TestOCPNFDContentCleanupRechecksRulesAfterConflict(t *testing.T) {
	ctx := context.Background()
	desired := cleanupNFDObject(cleanupNFDRules)
	initial := cleanupNFDObject(cleanupNFDRules + "\ncore: {sleepInterval: 2m}")
	attempts := 0
	c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			attempts++
			data, err := patch.Data(obj)
			require.NoError(t, err)
			require.Contains(t, string(data), "resourceVersion")
			if attempts == 1 {
				live := initial.DeepCopy()
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(initial), live))
				require.NoError(t, unstructured.SetNestedField(live.Object, cleanupNFDRules+"\ncore: {sleepInterval: 3m}\nsite: keep", "spec", "workerConfig", "configData"))
				require.NoError(t, c.Update(ctx, live))
				return apierrors.NewConflict(schema.GroupResource{Group: "nfd.openshift.io", Resource: "nodefeaturediscoveries"}, obj.GetName(), errors.New("site edit"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	_, err := cleanOCPNFDConfiguration(ctx, c, desired, initial)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	got := getRollbackObject(t, c, initial)
	raw, _, err := unstructured.NestedString(got.Object, "spec", "workerConfig", "configData")
	require.NoError(t, err)
	require.Contains(t, raw, "3m")
	require.Contains(t, raw, "site: keep")
	require.NotContains(t, raw, "deviceClassWhitelist")
}

func TestOCPNFDContentCleanupMissingInvalidAndChangedIdentity(t *testing.T) {
	for _, mode := range []string{"missing", "no manifest", "empty manifest", "invalid manifest", "get denied", "patch denied", "replacement", "deleting mixed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			desired := cleanupNFDObject(cleanupNFDRules)
			initial := cleanupNFDObject(cleanupNFDRules + "\nsite: keep")
			planned := initial.DeepCopy()
			switch mode {
			case "no manifest":
				desired = nil
			case "empty manifest":
				desired = cleanupNFDObject("")
			case "invalid manifest":
				desired = cleanupNFDObject("sources: [")
			case "replacement":
				initial.SetUID("replacement")
			case "deleting mixed":
				now := metav1.Now()
				initial.SetDeletionTimestamp(&now)
				initial.SetFinalizers([]string{"site/finalizer"})
			}
			builder := fake.NewClientBuilder()
			if mode != "missing" {
				builder = builder.WithObjects(initial)
			}
			c := builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if mode == "get denied" {
						return apierrors.NewForbidden(schema.GroupResource{Group: "nfd.openshift.io", Resource: "nodefeaturediscoveries"}, key.Name, errors.New("denied"))
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if mode == "patch denied" {
						return errors.New("patch denied")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					t.Fatal("invalid scope or mixed rules must not delete NFD")
					return nil
				},
			}).Build()
			_, err := cleanOCPNFDConfiguration(ctx, c, desired, planned)
			if mode == "missing" || mode == "no manifest" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestOCPNFDPreflightRejectsInvalidManifestAndOmitsUnselectedNFD(t *testing.T) {
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	initial := cleanupNFDObject(cleanupNFDRules)
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	objects, err := preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, nil, "cohort")
	require.NoError(t, err)
	require.Empty(t, objects)
	_, err = preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, []*unstructured.Unstructured{cleanupNFDObject("{}")}, "cohort")
	require.ErrorContains(t, err, "non-empty PCI")
	// YAML is valid but the selected field is not a string list.
	_, err = preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, []*unstructured.Unstructured{cleanupNFDObject(strings.ReplaceAll(cleanupNFDRules, "[vendor]", "wrong"))}, "cohort")
	require.ErrorContains(t, err, "deviceLabelFields")
}
