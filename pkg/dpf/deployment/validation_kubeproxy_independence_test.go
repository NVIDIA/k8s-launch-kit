// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

const proxyConfigPath = "/api/v1/namespaces/kube-system/configmaps/kube-proxy"
const proxySetPath = "/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy"
const proxyBindingPath = "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/kubeadm:node-proxier"

type kubeProxyDiagnosticTransport struct {
	tenantPendingTransport
	before func(string, int) *metav1.Status
}

func (r kubeProxyDiagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.before != nil {
		if status := r.before(request.URL.Path, r.reads[request.URL.Path]+1); status != nil {
			r.reads[request.URL.Path]++
			data, err := json.Marshal(status)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: int(status.Code), Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: request}, nil
		}
	}
	return r.tenantPendingTransport.RoundTrip(request)
}

func TestValidationKubeProxyIndependentChecks(t *testing.T) {
	for _, test := range []struct {
		name          string
		mutate        func(map[string]any)
		before        func(map[string]any, string, int) *metav1.Status
		configuration validation.State
		daemonset     validation.State
		pods          validation.State
		aggregate     validation.State
		drift         bool
	}{
		{name: "healthy", configuration: validation.Passed, daemonset: validation.Passed, pods: validation.Passed, aggregate: validation.Passed},
		{name: "empty CIDR does not hide healthy Pods", mutate: func(objects map[string]any) {
			config := objects[proxyConfigPath].(*corev1.ConfigMap)
			lines := strings.Split(config.Data["config.conf"], "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "clusterCIDR:") {
					lines[i] = "clusterCIDR: \"\""
				}
			}
			config.Data["config.conf"] = strings.Join(lines, "\n")
		}, configuration: validation.Failed, daemonset: validation.Passed, pods: validation.Passed, aggregate: validation.Failed},
		{name: "denied config does not hide healthy Pods", before: func(_ map[string]any, path string, _ int) *metav1.Status {
			if path == proxyConfigPath {
				return &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden, Message: "test reader cannot access configuration"}
			}
			return nil
		}, configuration: validation.Unverified, daemonset: validation.Passed, pods: validation.Passed, aggregate: validation.Unverified},
		{name: "rollout not ready does not hide healthy Pods", mutate: func(objects map[string]any) {
			set := objects[proxySetPath].(*appsv1.DaemonSet)
			set.Status.NumberReady, set.Status.NumberAvailable = 0, 0
			set.Status.NumberUnavailable = set.Status.DesiredNumberScheduled
		}, configuration: validation.Passed, daemonset: validation.Pending, pods: validation.Passed, aggregate: validation.Pending},
		{name: "foreign DaemonSet blocks Pod ownership", mutate: func(objects map[string]any) {
			set := objects[proxySetPath].(*appsv1.DaemonSet).DeepCopy()
			set.OwnerReferences[0].UID = "foreign"
			objects[proxySetPath] = set
		}, configuration: validation.Passed, daemonset: validation.Failed, pods: validation.Unverified, aggregate: validation.Failed},
		{name: "configuration drift revokes provisional results", before: func(objects map[string]any, path string, read int) *metav1.Status {
			if path == proxyConfigPath && read == 2 {
				objects[proxyConfigPath].(*corev1.ConfigMap).Data["config.conf"] += "hostnameOverride: changed\n"
			}
			return nil
		}, configuration: validation.Unverified, daemonset: validation.Unverified, pods: validation.Unverified, aggregate: validation.Unverified, drift: true},
		{name: "RBAC drift revokes provisional results", before: func(objects map[string]any, path string, read int) *metav1.Status {
			if path == proxyBindingPath && read == 2 {
				objects[proxyBindingPath].(*rbacv1.ClusterRoleBinding).Subjects[0].Name = "foreign"
			}
			return nil
		}, configuration: validation.Unverified, daemonset: validation.Unverified, pods: validation.Unverified, aggregate: validation.Unverified, drift: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o, _, _ := newTenantPendingObserver(t)
			f := o.fixture
			o.management.sources = func() (tenantSources, error) { return f.sources, nil }
			ctx := context.Background()
			endpoint, _, err := f.adapter.tenant.bootstrapCredentialSource(ctx)
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := f.adapter.native.validationSelectedTenantNodes(ctx, o.management, f.adapter.tenant)
			if err != nil {
				t.Fatal(err)
			}
			_, objects := tenantNetworkingObjects(t, f.adapter.native.desired.plan.Intent(), endpoint, nodes)
			if test.mutate != nil {
				test.mutate(objects)
			}
			reads := map[string]int{}
			base := f.adapter.tenant.api.CoreV1().RESTClient().Get().URL()
			f.adapter.tenant.api, err = kubernetes.NewForConfig(&rest.Config{Host: base.Scheme + "://" + base.Host, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter(), WrapTransport: func(next http.RoundTripper) http.RoundTripper {
				return kubeProxyDiagnosticTransport{tenantPendingTransport: tenantPendingTransport{next: next, objects: objects, reads: reads}, before: func(path string, read int) *metav1.Status {
					if test.before == nil {
						return nil
					}
					return test.before(objects, path, read)
				}}
			}})
			if err != nil {
				t.Fatal(err)
			}
			var completed []validation.ProgressEvent
			r := newValidationRecorder(f.adapter.native.systemValidationChecks(), func(event validation.ProgressEvent) {
				if event.Phase == validation.Completed {
					completed = append(completed, event)
				}
			})
			err = f.adapter.native.observeValidationKubeProxy(ctx, o.management, f.adapter.tenant, r)
			if validationIdentityChanged(err) != test.drift {
				t.Fatalf("identity change = %v; want %v; err=%v", validationIdentityChanged(err), test.drift, err)
			}
			wanted := map[validation.CheckID]validation.State{"tenant/kube-proxy/configuration": test.configuration, "tenant/kube-proxy/daemonset": test.daemonset, "tenant/kube-proxy": test.aggregate}
			for serial := range nodes {
				wanted[validation.CheckID("tenant/kube-proxy/device/"+serial)] = test.pods
			}
			for id, state := range wanted {
				outcome, ok := r.outcomes[id]
				if !ok || outcome.State != state {
					t.Errorf("%s = %+v; want %s", id, outcome, state)
				}
				if test.drift && outcome.Reason != validation.ReasonIdentityDrift {
					t.Errorf("%s lost identity drift: %+v", id, outcome)
				}
			}
			if test.pods == validation.Passed && reads["/api/v1/namespaces/kube-system/pods"] == 0 {
				t.Error("Pod pass without actual Pod read")
			}
			for _, event := range completed {
				if test.drift && event.Outcome.State == validation.Passed {
					t.Errorf("provisional pass escaped closing fence: %+v", event)
				}
			}
		})
	}
}
