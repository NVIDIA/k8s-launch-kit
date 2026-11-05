// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestValidateOCPNetworkAttachmentChecksNamespaceAndResource(t *testing.T) {
	network := ocpTestObject("sriovnetwork.openshift.io/v1", "SriovNetwork", "sriov-operator", "network-a", map[string]interface{}{
		"spec": map[string]interface{}{"networkNamespace": "workloads", "resourceName": "resource-a"},
	})
	require.True(t, isOCPSriovNetwork(network))
	ctx := context.Background()
	state, _ := validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().Build(), network)
	require.Equal(t, crstate.StateInProgress, state)

	nad := ocpTestObject("k8s.cni.cncf.io/v1", "NetworkAttachmentDefinition", "workloads", "network-a", nil)
	nad.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/resourceName": "nvidia.com/resource-a"})
	state, reason := validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().WithObjects(nad).Build(), network)
	require.Equal(t, crstate.StateError, state)
	require.Contains(t, reason, "openshift.io/resource-a")

	nad.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/resourceName": "openshift.io/resource-a"})
	state, reason = validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().WithObjects(nad).Build(), network)
	require.Equal(t, crstate.StateSuccess, state)
	require.Contains(t, reason, "workloads/network-a")

	missingNamespace := network.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(missingNamespace.Object, "", "spec", "networkNamespace"))
	state, _ = validateOCPNetworkAttachment(ctx, fake.NewClientBuilder().Build(), missingNamespace)
	require.Equal(t, crstate.StateError, state)
}

func TestValidateOCPPlatformSecondaryNetworkIsUnverified(t *testing.T) {
	installer := func(name, image string, selector map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform-networking"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: selector,
				Containers:   []corev1.Container{{Name: "installer", Image: image, VolumeMounts: []corev1.VolumeMount{{Name: "bin", MountPath: "/host/opt/cni/bin"}}}},
				Volumes:      []corev1.Volume{{Name: "bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin"}}}},
			}}},
		}
	}
	for _, tc := range []struct {
		name       string
		installers []client.Object
		listErr    error
		k8sMultus  crstate.CRState
		k8sPlugins crstate.CRState
	}{
		{name: "host-only installation"},
		{name: "multiple Multus installers", installers: []client.Object{installer("multus-a", "mirror/multus-cni:v1", nil), installer("multus-b", "mirror/multus-cni:v1", nil)}, k8sMultus: crstate.StateError},
		{name: "multiple CNI installers", installers: []client.Object{installer("plugins-a", "mirror/cni-plugins:v1", nil), installer("plugins-b", "mirror/plugins:v1", nil)}, k8sPlugins: crstate.StateError},
		{name: "reconciling installers", installers: []client.Object{installer("multus", "mirror/multus-cni:v1", nil), installer("plugins", "mirror/plugins:v1", nil)}, k8sMultus: crstate.StateInProgress, k8sPlugins: crstate.StateInProgress},
		{name: "installer selectors exclude worker", installers: []client.Object{installer("multus", "mirror/multus-cni:v1", map[string]string{"platform": "other"}), installer("plugins", "mirror/plugins:v1", map[string]string{"platform": "other"})}, k8sMultus: crstate.StateError, k8sPlugins: crstate.StateError},
		{name: "optional inspection denied", listErr: errors.New("list denied")},
	} {
		for _, flavor := range []string{config.FlavorOCP, config.FlavorK8s} {
			t.Run(tc.name+"/"+flavor, func(t *testing.T) {
				artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec: {}\n"}})
				require.NoError(t, err)
				live := artifacts.Documents()[0].ObjectCopy()
				require.NoError(t, unstructured.SetNestedField(live.Object, "ready", "status", "state"))
				scheme := runtime.NewScheme()
				require.NoError(t, appsv1.AddToScheme(scheme))
				require.NoError(t, corev1.AddToScheme(scheme))
				objects := append([]client.Object{live, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{"platform": "supported"}}}}, tc.installers...)
				installerLists := 0
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						installerLists++
						if tc.listErr != nil {
							return tc.listErr
						}
						return c.List(ctx, list, opts...)
					},
				}).Build()
				cfg := &config.LaunchKitConfig{Flavor: flavor, ClusterConfig: []config.ClusterConfig{{WorkerNodes: []string{"worker-a"}}}}
				results, err := ValidateBundleWithConfig(context.Background(), c, artifacts, cfg, flavor)
				require.NoError(t, err)
				require.Len(t, results, 3)
				for i, component := range []string{"multus", "cniPlugins"} {
					r := results[i]
					require.Equal(t, "ExternalSecondaryNetwork", r.Kind)
					require.Equal(t, component, r.Name)
					if flavor == config.FlavorOCP {
						require.True(t, r.Skipped)
						require.Empty(t, r.State)
						require.False(t, r.Found)
						require.False(t, r.Missing)
						require.Contains(t, r.Reason, "OpenShift")
						continue
					}
					want := tc.k8sMultus
					if component == "cniPlugins" {
						want = tc.k8sPlugins
					}
					require.Equal(t, want == "", r.Skipped)
					require.Equal(t, want, r.State)
				}
				require.Equal(t, "NicClusterPolicy", results[2].Kind)
				require.Equal(t, crstate.StateSuccess, results[2].State)
				if flavor == config.FlavorOCP {
					require.Zero(t, installerLists, "OpenShift platform installers are outside this check")
				} else {
					require.Positive(t, installerLists)
				}
			})
		}
	}
}

func TestValidateOCPRetainsPolicyReadinessAndOwnershipFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		liveState string
		managed   string
		wantState crstate.CRState
	}{
		{name: "policy error", liveState: "error", wantState: crstate.StateError},
		{name: "policy reconciling", liveState: "notReady", wantState: crstate.StateInProgress},
		{name: "managed Multus ownership conflict", liveState: "ready", managed: "multus", wantState: crstate.StateSuccess},
		{name: "managed plugins ownership conflict", liveState: "ready", managed: "cniPlugins", wantState: crstate.StateSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec: {}\n"}})
			require.NoError(t, err)
			live := artifacts.Documents()[0].ObjectCopy()
			require.NoError(t, unstructured.SetNestedField(live.Object, tc.liveState, "status", "state"))
			if tc.managed != "" {
				require.NoError(t, unstructured.SetNestedMap(live.Object, map[string]interface{}{}, "spec", "secondaryNetwork", tc.managed))
			}
			c := fake.NewClientBuilder().WithObjects(live).Build()
			results, err := ValidateBundle(context.Background(), c, artifacts, config.FlavorOCP)
			require.NoError(t, err)
			if tc.managed != "" {
				require.Len(t, results, 2)
				require.Equal(t, "SecondaryNetworkOwnership", results[0].Kind)
				require.Contains(t, results[0].Reason, "managed-to-external")
			} else {
				require.Len(t, results, 3)
				require.True(t, results[0].Skipped)
				require.True(t, results[1].Skipped)
			}
			require.False(t, results[0].Found)
			require.Equal(t, tc.wantState, results[len(results)-1].State)
			if tc.managed != "" {
				require.Equal(t, crstate.StateError, results[0].State)
			}
		})
	}
}
