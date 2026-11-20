// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// Keep test setup concise while exercising the same preparation/reset boundary
// the coordinator uses around upstream rail and network teardown.
func cleanSriovPolicies(ctx context.Context, c client.Client, refs []cleanObjectRef, ns string, interval time.Duration, out ui.Output) (int, error) {
	reset, err := prepareSriovReset(ctx, c, refs, ns, fakeSriovLogReader)
	if err != nil {
		return 0, err
	}
	return reset.deletePoliciesAndWait(ctx, c, interval, out)
}

func sriovCleanObject(kind, name string) *unstructured.Unstructured {
	return testCleanObject(schema.GroupVersionKind{Group: cleanSriovGroup, Version: "v1", Kind: kind}, "operator-system", name)
}

func sriovCleanState() *unstructured.Unstructured {
	obj := sriovCleanObject("SriovNetworkNodeState", "worker")
	obj.SetUID("worker-state")
	obj.SetGeneration(1)
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(100, 0)))
	obj.Object["spec"] = map[string]interface{}{"interfaces": []interface{}{}, "bridges": map[string]interface{}{}, "system": map[string]interface{}{}}
	obj.Object["status"] = map[string]interface{}{
		"syncStatus": "Succeeded", "lastSyncError": "",
		"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}},
	}
	return obj
}

func sriovCleanClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	for _, obj := range append([]client.Object{}, objects...) {
		if obj.GetObjectKind().GroupVersionKind().Kind == "SriovNetworkNodeState" {
			objects = append(objects, sriovDaemonPod(obj.GetName(), obj.GetNamespace()))
		}
	}
	for _, kind := range []string{"SriovNetworkNodePolicy", "SriovNetworkNodeState", "SriovNetworkPoolConfig", "SriovOperatorConfig"} {
		gvk := schema.GroupVersionKind{Group: cleanSriovGroup, Version: "v1", Kind: kind}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func sriovCleanRefs(objects ...client.Object) []cleanObjectRef {
	refs := make([]cleanObjectRef, 0, len(objects))
	for _, obj := range objects {
		refs = append(refs, cleanObjectRef{GVK: obj.GetObjectKind().GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()})
	}
	return refs
}

func TestCleanSriovStatePending(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*unstructured.Unstructured)
		pending string
		failure string
	}{
		{name: "empty serialized maps", mutate: func(*unstructured.Unstructured) {}},
		{name: "drain controller still returning node", mutate: func(obj *unstructured.Unstructured) {
			obj.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/current-state": "Drain_Complete", "sriovnetwork.openshift.io/desired-state": "Idle"})
		}, pending: "has not returned to Idle"},
		{name: "drain annotations idle", mutate: func(obj *unstructured.Unstructured) {
			obj.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/current-state": "Idle", "sriovnetwork.openshift.io/desired-state": "Idle"})
		}},
		{name: "old successful VF configuration", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}
		}, pending: "still has 8 VFs"},
		{name: "desired interfaces remain", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["spec"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}}
		}, pending: "desired spec is not empty"},
		{name: "pool desired system remains", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["spec"].(map[string]interface{})["system"] = map[string]interface{}{"rdmaMode": "exclusive"}
		}, pending: "desired spec is not empty"},
		{name: "desired bridges remain", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["spec"].(map[string]interface{})["bridges"] = map[string]interface{}{"ovs": []interface{}{map[string]interface{}{"name": "br0"}}}
		}, pending: "desired spec is not empty"},
		{name: "in progress", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["syncStatus"] = "InProgress"
		}, pending: "InProgress"},
		{name: "failed", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["syncStatus"] = "Failed"
			obj.Object["status"].(map[string]interface{})["lastSyncError"] = "reset rejected"
		}, failure: "reset rejected"},
		{name: "empty observed interfaces", mutate: func(obj *unstructured.Unstructured) {
			delete(obj.Object["status"].(map[string]interface{}), "interfaces")
		}, pending: "no observed interfaces"},
		{name: "externally managed VFs remain", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8), "externallyManaged": true}}
		}},
		{name: "omitted zero numVfs", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0"}}
		}},
		{name: "inconsistent VF inventory", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{
				"pciAddress": "0000:01:00.0", "numVfs": int64(0), "Vfs": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.1"}},
			}}
		}, pending: "still reports VF devices"},
		{name: "malformed observed numVfs", mutate: func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": "zero"}}
		}, failure: "int64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := sriovCleanState()
			tt.mutate(obj)
			pending, err := cleanSriovStatePending(obj)
			if tt.failure != "" {
				require.ErrorContains(t, err, tt.failure)
				return
			}
			require.NoError(t, err)
			if tt.pending == "" {
				assert.Empty(t, pending)
			} else {
				assert.Contains(t, pending, tt.pending)
			}
		})
	}
}

func TestCleanSriovPoliciesBatchAndWait(t *testing.T) {
	policy1 := sriovCleanObject("SriovNetworkNodePolicy", "p1")
	policy2 := sriovCleanObject("SriovNetworkNodePolicy", "p2")
	pool := sriovCleanObject("SriovNetworkPoolConfig", "pool")
	defaults := sriovCleanObject("SriovNetworkNodePolicy", "default")
	operator := sriovCleanObject("SriovOperatorConfig", "default")
	state := sriovCleanState()
	base := sriovCleanClient(policy1, policy2, pool, defaults, operator, state)
	refs := sriovCleanRefs(policy1, policy2, pool, defaults, operator, state)
	var deleted []string
	polls := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return underlying.Delete(ctx, obj, opts...)
		},
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			err := underlying.List(ctx, list, opts...)
			if list.GetObjectKind().GroupVersionKind().Kind != "SriovNetworkNodeStateList" || err != nil {
				return err
			}
			polls++
			if polls == 1 {
				return nil
			}
			// Every intent deletion precedes polling. Controllers and their
			// NodeStates must survive while physical state is still stale.
			assert.Equal(t, []string{"p1", "p2", "pool"}, deleted)
			for _, obj := range []client.Object{defaults, operator, state} {
				assert.NoError(t, underlying.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object)))
			}
			observed := &list.(*unstructured.UnstructuredList).Items[0]
			switch polls {
			case 2:
				// Policy deletion has not yet reached the node spec.
				observed.Object["spec"] = map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}}
			case 3:
				// The spec is empty but Succeeded still describes old hardware.
				observed.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}
			case 4:
				// Physical state alone does not waive a pending sync.
				observed.Object["status"].(map[string]interface{})["syncStatus"] = "InProgress"
			}
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	count, err := cleanSriovPolicies(ctx, c, refs, "operator-system", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.GreaterOrEqual(t, polls, 5)
}

func TestCleanSriovPoliciesRetryAndInspectionFailure(t *testing.T) {
	t.Run("intent without node states fails before mutation", func(t *testing.T) {
		policy := sriovCleanObject("SriovNetworkNodePolicy", "p")
		c := sriovCleanClient(policy)
		count, err := cleanSriovPolicies(context.Background(), c, sriovCleanRefs(policy), "operator-system", time.Millisecond, ui.NewSilent())
		require.ErrorContains(t, err, "no SriovNetworkNodeStates")
		assert.Zero(t, count)
		assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy))
	})
	t.Run("managed PF cannot disappear from host inventory", func(t *testing.T) {
		state := sriovCleanState()
		base := sriovCleanClient(state)
		lists := 0
		c := interceptor.NewClient(base, interceptor.Funcs{
			List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := underlying.List(ctx, list, opts...); err != nil {
					return err
				}
				lists++
				if lists > 1 {
					list.(*unstructured.UnstructuredList).Items[0].Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(0)}}
				}
				return nil
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := cleanSriovPolicies(ctx, c, sriovCleanRefs(state), "operator-system", time.Millisecond, ui.NewSilent())
		require.ErrorContains(t, err, "managed PF 0000:01:00.0 missing")
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("retry waits with policies already absent", func(t *testing.T) {
		state := sriovCleanState()
		state.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		count, err := cleanSriovPolicies(ctx, sriovCleanClient(state), sriovCleanRefs(state), "operator-system", time.Millisecond, ui.NewSilent())
		assert.Zero(t, count)
		require.ErrorContains(t, err, "still has 8 VFs")
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("inspection fails before deleting policy", func(t *testing.T) {
		policy := sriovCleanObject("SriovNetworkNodePolicy", "p")
		base := sriovCleanClient(policy)
		deleted := false
		c := interceptor.NewClient(base, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("forbidden")
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				deleted = true
				return nil
			},
		})
		count, err := cleanSriovPolicies(context.Background(), c, sriovCleanRefs(policy), "operator-system", time.Millisecond, ui.NewSilent())
		require.ErrorContains(t, err, "forbidden")
		assert.Zero(t, count)
		assert.False(t, deleted)
	})
	t.Run("baseline state disappearance is not reset proof", func(t *testing.T) {
		state := sriovCleanState()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		count, err := cleanSriovPolicies(ctx, sriovCleanClient(), sriovCleanRefs(state), "operator-system", time.Millisecond, ui.NewSilent())
		require.ErrorContains(t, err, "disappeared before reset baseline")
		assert.Zero(t, count)
	})
	t.Run("no SRIOV resources needs no SRIOV API", func(t *testing.T) {
		count, err := cleanSriovPolicies(context.Background(), sriovCleanClient(), nil, "operator-system", time.Millisecond, ui.NewSilent())
		require.NoError(t, err)
		assert.Zero(t, count)
	})
}

func sriovDaemonPod(node, namespace string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "daemon-" + node, Namespace: namespace, UID: "daemon-pod", Labels: map[string]string{"app": cleanSriovDaemonContainer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: cleanSriovDaemonContainer, UID: "daemon-set", Controller: ptrTrue()}}},
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: cleanSriovDaemonContainer}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: cleanSriovDaemonContainer, ContainerID: "runtime://daemon", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Unix(101, 0))}}}}}}
}

func sriovGenerationLog(node, namespace, id string, generation int64, succeeded bool) []byte {
	line := fmt.Sprintf("1970-01-01T00:03:20Z 1970-01-01T00:03:20Z\tINFO\tReconcile\tnew generation\t{\"name\":%q,\"namespace\":%q,\"reconcileID\":%q,\"generation\":%d}\n", node, namespace, id, generation)
	if succeeded {
		line += fmt.Sprintf("1970-01-01T00:03:21Z 1970-01-01T00:03:21Z\tINFO\tApply\tsync succeeded\t{\"name\":%q,\"namespace\":%q,\"reconcileID\":%q}\n", node, namespace, id)
	}
	return []byte(line)
}

func fakeSriovLogReader(_ context.Context, namespace, pod, _ string, _ metav1.Time) ([]byte, error) {
	node := strings.TrimPrefix(pod, "daemon-")
	return append(sriovGenerationLog(node, namespace, "r1", 1, true), sriovGenerationLog(node, namespace, "r2", 2, true)...), nil
}

func TestCleanSriovGenerationLogProof(t *testing.T) {
	state := sriovCleanState()
	state.SetGeneration(2)
	valid := sriovGenerationLog(state.GetName(), state.GetNamespace(), "r", 2, true)
	tests := []struct {
		name  string
		log   []byte
		valid bool
	}{
		{name: "captured generation and same reconcile", log: valid, valid: true},
		{name: "old captured generation", log: sriovGenerationLog(state.GetName(), state.GetNamespace(), "r", 1, true)},
		{name: "incomplete apply", log: sriovGenerationLog(state.GetName(), state.GetNamespace(), "r", 2, false)},
		{name: "wrong node", log: sriovGenerationLog("other", state.GetNamespace(), "r", 2, true)},
		{name: "wrong namespace", log: sriovGenerationLog(state.GetName(), "other", "r", 2, true)},
		{name: "different reconcile ids", log: []byte(strings.Replace(string(valid), `"reconcileID":"r"}`, `"reconcileID":"another"}`, 1))},
		{name: "old lifetime", log: []byte(strings.ReplaceAll(string(valid), "00:03:2", "00:00:2"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.valid, sriovLogProvesGeneration(tt.log, state, time.Unix(101, 0)))
		})
	}
	jsonLog := []byte("1970-01-01T00:03:20Z {\"msg\":\"new generation\",\"name\":\"worker\",\"namespace\":\"operator-system\",\"reconcileID\":\"json\",\"generation\":2}\n1970-01-01T00:03:21Z {\"msg\":\"sync succeeded\",\"name\":\"worker\",\"namespace\":\"operator-system\",\"reconcileID\":\"json\"}\n")
	assert.True(t, sriovLogProvesGeneration(jsonLog, state, time.Unix(101, 0)))
}

func TestCleanSriovRequiresLogsForNonpristineState(t *testing.T) {
	state := sriovCleanState()
	state.SetGeneration(2)
	c := sriovCleanClient(state)
	_, err := prepareSriovReset(context.Background(), c, sriovCleanRefs(state), state.GetNamespace(), nil)
	require.ErrorContains(t, err, "Pod log reader")
	_, err = prepareSriovReset(context.Background(), c, sriovCleanRefs(state), state.GetNamespace(), fakeSriovLogReader)
	require.NoError(t, err)
	_, err = verifySriovDaemonGeneration(context.Background(), c, state, func(context.Context, string, string, string, metav1.Time) ([]byte, error) {
		return nil, fmt.Errorf("forbidden")
	})
	require.ErrorContains(t, err, "forbidden")
	_, err = verifySriovDaemonGeneration(context.Background(), c, state, func(context.Context, string, string, string, metav1.Time) ([]byte, error) {
		return sriovGenerationLog(state.GetName(), state.GetNamespace(), "old-apply", 1, true), nil
	})
	require.NoError(t, err)
}

func TestCleanSriovRejectsDaemonRestartDuringLogRead(t *testing.T) {
	state := sriovCleanState()
	state.SetGeneration(2)
	c := sriovCleanClient(state)
	_, err := verifySriovDaemonGeneration(context.Background(), c, state, func(ctx context.Context, namespace, podName, _ string, _ metav1.Time) ([]byte, error) {
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); err != nil {
			return nil, err
		}
		pod.Status.ContainerStatuses[0].RestartCount++
		if err := c.Status().Update(ctx, pod); err != nil {
			return nil, err
		}
		return sriovGenerationLog(state.GetName(), namespace, "r", 2, true), nil
	})
	require.ErrorContains(t, err, "container changed")
}

func ptrTrue() *bool { value := true; return &value }

func TestCleanSriovMissingLogProofWaitsAndThrottles(t *testing.T) {
	state := sriovCleanState()
	state.SetGeneration(2)
	c := sriovCleanClient(state)
	reads := 0
	reader := func(context.Context, string, string, string, metav1.Time) ([]byte, error) { reads++; return nil, nil }
	reset, err := prepareSriovReset(context.Background(), c, sriovCleanRefs(state), state.GetNamespace(), reader)
	require.NoError(t, err)
	node := reset.nodes[state.GetName()]
	for range 3 {
		reason, err := node.pending(context.Background(), c, state, reader)
		require.NoError(t, err)
		assert.Contains(t, reason, "waiting for captured generation 2")
	}
	assert.Equal(t, 1, reads)
	node.nextLogRead = time.Now().Add(-time.Second)
	reader = func(context.Context, string, string, string, metav1.Time) ([]byte, error) {
		reads++
		return sriovGenerationLog(state.GetName(), state.GetNamespace(), "r", 2, true), nil
	}
	reason, err := node.pending(context.Background(), c, state, reader)
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, 2, reads)
	reason, err = node.pending(context.Background(), c, state, reader)
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, 2, reads)
}

func TestCleanSriovPristineSiblingNeedsNoLogs(t *testing.T) {
	state := sriovCleanState()
	policy := sriovCleanObject("SriovNetworkNodePolicy", "sibling")
	c := sriovCleanClient(state, policy)
	reset, err := prepareSriovReset(context.Background(), c, sriovCleanRefs(state, policy), state.GetNamespace(), nil)
	require.NoError(t, err)
	assert.False(t, reset.nodes[state.GetName()].requiresProof)
	count, err := reset.deletePoliciesAndWait(context.Background(), c, time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestCleanSriovPristineNodeEscalatesAfterGenerationChange(t *testing.T) {
	state := sriovCleanState()
	c := sriovCleanClient(state)
	reads := 0
	reader := func(context.Context, string, string, string, metav1.Time) ([]byte, error) {
		reads++
		return sriovGenerationLog(state.GetName(), state.GetNamespace(), "r", 2, true), nil
	}
	reset, err := prepareSriovReset(context.Background(), c, sriovCleanRefs(state), state.GetNamespace(), reader)
	require.NoError(t, err)
	state.SetGeneration(2)
	require.NoError(t, c.Update(context.Background(), state))
	_, err = reset.deletePoliciesAndWait(context.Background(), c, time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.True(t, reset.nodes[state.GetName()].requiresProof)
	assert.Equal(t, 1, reads)
}

func TestCleanSriovRejectsUnrelatedDaemonPod(t *testing.T) {
	pod := sriovDaemonPod("worker", "operator-system")
	pod.OwnerReferences[0].Name = "unrelated-daemon"
	_, err := sriovDaemonIdentity(pod)
	require.ErrorContains(t, err, "not owned by the SR-IOV config daemon")
}

func TestCleanSriovRechecksAllStateAfterLogRead(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*unstructured.Unstructured)
		pending string
		failure string
	}{
		{name: "same-generation failure", change: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]interface{})["syncStatus"] = "Failed"
		}, failure: "reset failed"},
		{name: "same-generation in progress", change: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]interface{})["syncStatus"] = "InProgress"
		}, pending: "InProgress"},
		{name: "VFs reappear", change: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}
		}, pending: "still has 8 VFs"},
		{name: "drain resumes", change: func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/current-state": "Draining"})
		}, pending: "has not returned to Idle"},
		{name: "managed PF disappears", change: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]interface{})["interfaces"] = []interface{}{map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(0)}}
		}, pending: "managed PF 0000:01:00.0 missing"},
		{name: "managed bridge reappears", change: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]interface{})["bridges"] = map[string]interface{}{"ovs": []interface{}{map[string]interface{}{"name": "br0"}}}
		}, pending: "managed bridge br0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := sriovCleanState()
			state.SetGeneration(2)
			c := sriovCleanClient(state)
			reader := func(ctx context.Context, namespace, pod, container string, since metav1.Time) ([]byte, error) {
				latest := state.DeepCopy()
				if err := c.Get(ctx, client.ObjectKeyFromObject(state), latest); err != nil {
					return nil, err
				}
				tt.change(latest)
				if err := c.Update(ctx, latest); err != nil {
					return nil, err
				}
				return sriovGenerationLog(state.GetName(), namespace, "r", 2, true), nil
			}
			reset, err := prepareSriovReset(context.Background(), c, sriovCleanRefs(state), state.GetNamespace(), reader)
			require.NoError(t, err)
			node := reset.nodes[state.GetName()]
			node.bridges["br0"] = struct{}{}
			reason, err := node.pending(context.Background(), c, state, reader)
			if tt.failure != "" {
				require.ErrorContains(t, err, tt.failure)
			} else {
				require.NoError(t, err)
				assert.Contains(t, reason, tt.pending)
			}
			assert.Zero(t, node.provedGeneration, "changed API state must not be cached as verified")
		})
	}
}
