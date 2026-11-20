// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func shutdownTestPod() *corev1.Pod {
	controller := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "driver-worker", Namespace: "operator-system", UID: types.UID("driver-1"), Labels: map[string]string{mofedDriverLabel: ""}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "mofed-ds", UID: types.UID("ds-1"), Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: mofedDriverContainer, Image: "driver:test"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: mofedDriverContainer, ContainerID: "containerd://driver-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
}

func shutdownTerminal(pod *corev1.Pod, exitCode int32) *corev1.Pod {
	pod = pod.DeepCopy()
	deleting := metav1.NewTime(time.Unix(100, 0))
	pod.DeletionTimestamp = &deleting
	pod.ResourceVersion = "11"
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, FinishedAt: metav1.NewTime(time.Unix(101, 0))}}
	return pod
}

func shutdownTestClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
	gomega.Expect(appsv1.AddToScheme(scheme)).To(gomega.Succeed())
	for _, kind := range []string{"NicClusterPolicy", "NicNodePolicy"} {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	namespacedPodLists := 0
	return interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if pods, ok := list.(*corev1.PodList); ok {
			resolved := &client.ListOptions{}
			resolved.ApplyOptions(opts)
			pods.ResourceVersion = "10"
			if resolved.Namespace != "" {
				namespacedPodLists++
				if namespacedPodLists > 1 {
					pods.ResourceVersion = "100"
				}
			}
		}
		return nil
	}})
}

var _ = ginkgo.Describe("verified MOFED shutdown", func() {
	ginkgo.It("requires successful terminal evidence for the current instance during deletion", func() {
		pod := shutdownTestPod()
		observer := mofedShutdownObserver{pods: map[types.UID]*mofedShutdownPod{}}
		gomega.Expect(observer.observe(pod, false)).To(gomega.Succeed())
		gomega.Expect(observer.complete()).To(gomega.BeFalse())
		terminal := shutdownTerminal(pod, 0)
		graceDeadline := metav1.NewTime(time.Unix(400, 0))
		terminal.DeletionTimestamp = &graceDeadline
		gomega.Expect(observer.observe(terminal, true)).To(gomega.Succeed())
		gomega.Expect(observer.complete()).To(gomega.BeTrue())
	})
	ginkgo.DescribeTable("rejects incomplete or failed terminal evidence", func(change func(*corev1.Pod), expected string) {
		pod := shutdownTestPod()
		observer := mofedShutdownObserver{pods: map[types.UID]*mofedShutdownPod{}}
		gomega.Expect(observer.observe(pod, false)).To(gomega.Succeed())
		terminal := shutdownTerminal(pod, 0)
		change(terminal)
		gomega.Expect(observer.observe(terminal, true)).To(gomega.MatchError(gomega.ContainSubstring(expected)))
	},
		ginkgo.Entry("SIGKILL", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 137
			p.Status.ContainerStatuses[0].State.Terminated.Signal = 9
		}, "failed shutdown"),
		ginkgo.Entry("legacy bash exit", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1 }, "failed shutdown"),
		ginkgo.Entry("force deletion", func(p *corev1.Pod) { p.Status.ContainerStatuses = nil }, "without successful"),
		ginkgo.Entry("missing completion time", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} }, "completion timestamp"),
		ginkgo.Entry("restart", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 }, "changed driver container instance"),
		ginkgo.Entry("replacement container", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "containerd://new" }, "changed driver container instance"),
	)
	ginkgo.It("tracks replacements by UID and ignores unrelated namespace pods", func() {
		pod := shutdownTestPod()
		observer := mofedShutdownObserver{pods: map[types.UID]*mofedShutdownPod{}}
		gomega.Expect(observer.observe(shutdownTerminal(pod, 0), true)).To(gomega.Succeed())
		replacement := pod.DeepCopy()
		replacement.UID = "driver-2"
		gomega.Expect(observer.event(watch.Event{Type: watch.Added, Object: replacement})).To(gomega.Succeed())
		gomega.Expect(observer.complete()).To(gomega.BeFalse())
		unrelated := pod.DeepCopy()
		unrelated.UID = "unrelated"
		unrelated.OwnerReferences = nil
		gomega.Expect(observer.observe(unrelated, true)).To(gomega.Succeed())
		gomega.Expect(observer.pods).To(gomega.HaveLen(2))
	})
	ginkgo.It("creates the checkpoint before deleting and removes it only after observed successful shutdown", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			resolved := &client.ListOptions{}
			resolved.ApplyOptions(opts)
			gomega.Expect(resolved.Raw.ResourceVersion).To(gomega.Equal("10"))
			gomega.Expect(resolved.Raw.AllowWatchBookmarks).To(gomega.BeTrue())
			return stream, nil
		}})
		count, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(ctx context.Context) (int, error) {
			checkpoint := &corev1.ConfigMap{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint); err != nil {
				return 0, err
			}
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Modify(shutdownTerminal(pod, 0))
			stream.Delete(shutdownTerminal(pod, 0))
			stream.Action(watch.Bookmark, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}})
			return 3, nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(3))
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(client.IgnoreNotFound(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint))).To(gomega.Succeed())
		gomega.Expect(checkpoint.Name).To(gomega.BeEmpty())
	})
	ginkgo.DescribeTable("preserves checkpoint and cancels deletion when observation fails", func(fail func(*watch.RaceFreeFakeWatcher, *corev1.Pod)) {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		count, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(ctx context.Context) (int, error) { fail(stream, pod); <-ctx.Done(); return 2, ctx.Err() })
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(2))
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
		called := false
		_, err = withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("incomplete attempt")))
		gomega.Expect(called).To(gomega.BeFalse())
	},
		ginkgo.Entry("closed stream", func(w *watch.RaceFreeFakeWatcher, _ *corev1.Pod) { w.Stop() }),
		ginkgo.Entry("expired stream", func(w *watch.RaceFreeFakeWatcher, _ *corev1.Pod) {
			w.Error(&metav1.Status{Status: "Failure", Reason: metav1.StatusReasonExpired, Code: 410, Message: "expired"})
		}),
		ginkgo.Entry("lost terminal state", func(w *watch.RaceFreeFakeWatcher, p *corev1.Pod) { w.Delete(p) }),
		ginkgo.Entry("failed shutdown", func(w *watch.RaceFreeFakeWatcher, p *corev1.Pod) { w.Delete(shutdownTerminal(p, 1)) }),
	)
	ginkgo.It("does not invoke deletion if watching is forbidden", func() {
		c := interceptor.NewClient(shutdownTestClient(shutdownTestPod()), interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return nil, fmt.Errorf("forbidden")
		}})
		called := false
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, "operator-system", func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("forbidden")))
		gomega.Expect(called).To(gomega.BeFalse())
	})
	ginkgo.It("does not create a checkpoint when no driver exists or is configured", func() {
		c := shutdownTestClient()
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, "operator-system", func(context.Context) (int, error) { return 0, nil })
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cms := &corev1.ConfigMapList{}
		gomega.Expect(c.List(context.Background(), cms)).To(gomega.Succeed())
		gomega.Expect(cms.Items).To(gomega.BeEmpty())
	})
	ginkgo.It("refuses an unobservable desired driver before deletion", func() {
		policy := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "policy"}, "spec": map[string]interface{}{"ofedDriver": map[string]interface{}{}}}}
		c := shutdownTestClient(policy)
		called := false
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, "operator-system", func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no driver Pod or DaemonSet")))
		gomega.Expect(called).To(gomega.BeFalse())
	})
	ginkgo.It("refuses drivers outside the namespace before cluster policy deletion", func() {
		pod := shutdownTestPod()
		pod.Namespace = "other-system"
		c := shutdownTestClient(pod)
		called := false
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, "operator-system", func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("outside cleanup namespace")))
		gomega.Expect(called).To(gomega.BeFalse())
	})

	ginkgo.It("verifies newly created and deleted driver UIDs before accepting the final empty inventory", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(ctx context.Context) (int, error) {
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Delete(shutdownTerminal(pod, 0))
			replacement := pod.DeepCopy()
			replacement.UID = "replacement"
			stream.Add(replacement)
			stream.Delete(replacement)
			return 1, nil
		})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("without successful driver shutdown evidence")))
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
	})
	ginkgo.It("retains failure evidence for an already terminating driver before invoking deletion", func() {
		pod := shutdownTerminal(shutdownTestPod(), 1)
		pod.Finalizers = []string{"example.com/retain"}
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		called := false
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed shutdown")))
		gomega.Expect(called).To(gomega.BeFalse())
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
	})
	ginkgo.It("does not delete policies when checkpoint creation is forbidden", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{
			Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
				return stream, nil
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return fmt.Errorf("checkpoint forbidden")
			},
		})
		called := false
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(context.Context) (int, error) { called = true; return 0, nil })
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("checkpoint forbidden")))
		gomega.Expect(called).To(gomega.BeFalse())
	})

	ginkgo.It("waits for a delayed bookmark covering the final empty pod list", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		finalList := make(chan struct{}, 1)
		c := interceptor.NewClient(base, interceptor.Funcs{
			Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
				return stream, nil
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if pods, ok := list.(*corev1.PodList); ok && pods.ResourceVersion == "100" {
					select {
					case finalList <- struct{}{}:
					default:
					}
				}
				return nil
			},
		})
		bookmarked := make(chan struct{})
		go func() {
			<-finalList
			stream.Action(watch.Bookmark, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}})
			close(bookmarked)
		}()
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(ctx context.Context) (int, error) {
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Delete(shutdownTerminal(pod, 0))
			return 1, nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Eventually(bookmarked).Should(gomega.BeClosed())
	})
	ginkgo.It("fails with a retained checkpoint when the watch never reaches the empty inventory fence", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := withVerifiedMOFEDShutdown(ctx, c, pod.Namespace, func(ctx context.Context) (int, error) {
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Delete(shutdownTerminal(pod, 0))
			return 1, nil
		})
		gomega.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(`watch fence "100"`))
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
	})
	ginkgo.It("rejects incomparable opaque watch fences instead of assuming progress", func() {
		reached, err := mofedWatchReached("opaque-one", "opaque-two")
		gomega.Expect(reached).To(gomega.BeFalse())
		gomega.Expect(err).To(gomega.HaveOccurred())
		reached, err = mofedWatchReached("opaque-one", "opaque-one")
		gomega.Expect(reached).To(gomega.BeTrue())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("rejects a delayed failed replacement delivered after the empty inventory", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		finalList := make(chan struct{}, 1)
		c := interceptor.NewClient(base, interceptor.Funcs{
			Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
				return stream, nil
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if pods, ok := list.(*corev1.PodList); ok && pods.ResourceVersion == "100" {
					select {
					case finalList <- struct{}{}:
					default:
					}
				}
				return nil
			},
		})
		delivered := make(chan struct{})
		go func() {
			<-finalList
			replacement := pod.DeepCopy()
			replacement.UID = "replacement"
			replacement.ResourceVersion = "50"
			stream.Add(replacement)
			failed := shutdownTerminal(replacement, 1)
			failed.ResourceVersion = "51"
			stream.Delete(failed)
			close(delivered)
		}()
		_, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(ctx context.Context) (int, error) {
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Delete(shutdownTerminal(pod, 0))
			return 1, nil
		})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed shutdown")))
		gomega.Eventually(delivered).Should(gomega.BeClosed())
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
	})
	ginkgo.It("propagates callback failure and retains its checkpoint", func() {
		pod := shutdownTestPod()
		base := shutdownTestClient(pod)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		failed := errors.New("policy deletion failed")
		count, err := withVerifiedMOFEDShutdown(context.Background(), c, pod.Namespace, func(context.Context) (int, error) { return 1, failed })
		gomega.Expect(errors.Is(err, failed)).To(gomega.BeTrue())
		gomega.Expect(count).To(gomega.Equal(1))
		checkpoint := &corev1.ConfigMap{}
		gomega.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: mofedCleanupCheckpoint}, checkpoint)).To(gomega.Succeed())
	})
})
