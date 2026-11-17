// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

func TestTenantHBNExecFixedCommandsAndSelection(t *testing.T) {
	origin, err := url.Parse("https://192.0.2.10:30538")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind   hbnRuntimeCommand
		prefix netip.Prefix
		argv   []string
	}{
		{kind: hbnReadGateway, argv: []string{"/usr/bin/nv", "show", "interface", "pf0hpf_if", "-o", "json"}},
		{kind: hbnReadBGPSummary, argv: []string{"/usr/bin/vtysh", "-c", "show bgp vrf default ipv4 unicast summary json"}},
		{kind: hbnReadRemoteRoute, prefix: netip.MustParsePrefix("10.0.120.8/29"), argv: []string{"/usr/bin/vtysh", "-c", "show ip route 10.0.120.8/29 json"}},
	} {
		endpoint, err := tenantHBNCommandURL(origin, "tenant-workloads", "hbn-pod", tc.kind, tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		query := endpoint.Query()
		if endpoint.Host != origin.Host || endpoint.Path != "/api/v1/namespaces/tenant-workloads/pods/hbn-pod/exec" ||
			!reflect.DeepEqual(query["command"], tc.argv) || query.Get("container") != "doca-hbn" ||
			query.Get("stdout") != "true" || query.Get("stderr") != "true" || query.Has("stdin") || query.Has("tty") || len(query) != 4 {
			t.Fatalf("unexpected command boundary: %s", endpoint)
		}
	}
	for _, tc := range []struct {
		ns, pod string
		kind    hbnRuntimeCommand
		prefix  netip.Prefix
	}{
		{ns: "other/ns", pod: "pod", kind: hbnReadGateway},
		{ns: "tenant", pod: "../pod", kind: hbnReadGateway},
		{ns: "tenant", pod: "pod?command=sh", kind: hbnReadGateway},
		{ns: "tenant", pod: "pod", kind: 255},
		{ns: "tenant", pod: "pod", kind: 0},
		{ns: "tenant", pod: "pod", kind: hbnReadRemoteRoute},
		{ns: "tenant", pod: "pod", kind: hbnReadRemoteRoute, prefix: netip.MustParsePrefix("10.0.120.9/29")},
		{ns: "tenant", pod: "pod", kind: hbnReadRemoteRoute, prefix: netip.MustParsePrefix("::/64")},
		{ns: "tenant", pod: "pod", kind: hbnReadBGPSummary, prefix: netip.MustParsePrefix("10.0.120.8/29")},
	} {
		if _, err := tenantHBNCommandURL(origin, tc.ns, tc.pod, tc.kind, tc.prefix); err == nil {
			t.Fatalf("accepted unsupported command/input: %+v", tc)
		}
	}
	if origin.Path != "" || origin.RawQuery != "" {
		t.Fatal("building exec URL mutated the retained origin")
	}
}

type tenantCommandTestExecutor struct {
	run func(context.Context, remotecommand.StreamOptions) error
}

func (e tenantCommandTestExecutor) Stream(options remotecommand.StreamOptions) error {
	return e.run(context.Background(), options)
}

func (e tenantCommandTestExecutor) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return e.run(ctx, options)
}

func TestTenantHBNExecCaptureRejectsPartialAndUnboundedStreams(t *testing.T) {
	for _, tc := range []struct {
		name, output, diagnostic, want string
		failure                        bool
		cancel                         bool
	}{
		{name: "clean JSON", output: "{\"as\":65101}"},
		{name: "empty", want: "RESPONSE_INVALID"},
		{name: "diagnostic", output: "{}", diagnostic: "private-diagnostic-canary", want: "RESPONSE_INVALID"},
		{name: "partial stdout on error", output: "{}", failure: true, want: "EXEC_FAILED"},
		{name: "stdout overflow", output: strings.Repeat("x", tenantCommandOutputLimit+1), want: "OUTPUT_LIMIT"},
		{name: "stderr overflow", diagnostic: strings.Repeat("x", tenantCommandOutputLimit+1), want: "OUTPUT_LIMIT"},
		{name: "canceled", output: "{}", cancel: true, want: "CANCELED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			executor := tenantCommandTestExecutor{run: func(_ context.Context, options remotecommand.StreamOptions) error {
				if options.Stdin != nil || options.Tty || options.TerminalSizeQueue != nil {
					t.Fatal("unexpected input/TTY stream")
				}
				if tc.output != "" {
					_, _ = options.Stdout.Write([]byte(tc.output))
				}
				if tc.diagnostic != "" {
					_, _ = options.Stderr.Write([]byte(tc.diagnostic))
				}
				if tc.cancel {
					cancel()
				}
				if tc.failure {
					return fmt.Errorf("private-exec-error-canary")
				}
				return nil
			}}
			output, err := captureTenantHBNCommand(ctx, cancel, executor)
			if tc.want == "" {
				if err != nil || string(output) != tc.output {
					t.Fatalf("output=%q err=%v", output, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || len(output) != 0 || strings.Contains(err.Error(), "canary") {
				t.Fatalf("unsafe partial/error output=%q err=%v", output, err)
			}
		})
	}
}

func TestTenantHBNExecRejectsRedirectAndAmbientProxy(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/tenant/pods/hbn-pod/exec" || r.Header.Get("Authorization") != "Bearer tenant-exec-canary" {
			t.Error("unexpected authenticated exec request")
		}
		http.Redirect(w, r, other.URL+"/must-not-follow", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv("HTTPS_PROXY", "http://192.0.2.200:1")
	t.Setenv("NO_PROXY", "")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	var configuredProxy atomic.Int32
	config := &rest.Config{Host: server.URL, Timeout: time.Second, BearerToken: "tenant-exec-canary", TLSClientConfig: rest.TLSClientConfig{CAData: ca},
		Proxy: func(*http.Request) (*url.URL, error) {
			configuredProxy.Add(1)
			return url.Parse("http://192.0.2.201:1")
		}}
	exec, err := newTenantPodExec(config)
	if err != nil {
		t.Fatal(err)
	}
	// Construction snapshots embedded trust instead of retaining the caller's slice.
	for i := range ca {
		ca[i] = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.read(ctx, "tenant", "hbn-pod", hbnReadBGPSummary, netip.Prefix{})
	if err == nil || len(output) != 0 || strings.Contains(err.Error(), "canary") || requests.Load() != 1 || redirected.Load() != 0 || configuredProxy.Load() != 0 {
		t.Fatalf("redirect/proxy policy not preserved: err=%v requests=%d redirected=%d proxy=%d", err, requests.Load(), redirected.Load(), configuredProxy.Load())
	}
	if config.Proxy == nil {
		t.Fatal("constructor mutated caller config")
	}
}

func TestTenantHBNExecPreservesTypedFailureReasons(t *testing.T) {
	resource := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		failure error
		want    string
	}{
		{failure: apierrors.NewForbidden(resource, "private-canary", fmt.Errorf("private-canary")), want: "FORBIDDEN"},
		{failure: apierrors.NewUnauthorized("private-canary"), want: "UNAUTHORIZED"},
		{failure: apierrors.NewNotFound(resource, "private-canary"), want: "POD_UNAVAILABLE"},
		{failure: utilexec.CodeExitError{Err: fmt.Errorf("private-canary"), Code: 127}, want: "TOOL_MISSING"},
		{failure: utilexec.CodeExitError{Err: fmt.Errorf("private-canary"), Code: 126}, want: "TOOL_NOT_EXECUTABLE"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		executor := tenantCommandTestExecutor{run: func(context.Context, remotecommand.StreamOptions) error { return tc.failure }}
		output, err := captureTenantHBNCommand(ctx, cancel, executor)
		cancel()
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) || strings.Contains(err.Error(), "canary") || len(output) != 0 {
			t.Fatalf("failure reason lost or leaked: %q %v", output, err)
		}
	}
}
