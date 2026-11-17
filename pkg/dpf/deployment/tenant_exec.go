// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	utilexec "k8s.io/client-go/util/exec"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const tenantCommandOutputLimit = 1 << 20

type hbnRuntimeCommand uint8

const (
	hbnReadGateway hbnRuntimeCommand = iota + 1
	hbnReadBGPSummary
	hbnReadRemoteRoute
)

// tenantPodExec retains only transport derived from the same validated embedded
// tenant configuration as tenantClient.api. Its caller owns current tenant,
// selected Pod UID/owner/node/image and lease fences around these read commands.
// No general pod command or second kubeconfig is exposed.
type tenantPodExec struct {
	origin    *url.URL
	transport http.RoundTripper
	upgrader  spdy.Upgrader
	timeout   time.Duration
}

// newTenantPodExec must receive the result of tenantRESTConfig. Constructing the
// upgrade transport does not connect to the API or execute a command.
func newTenantPodExec(config *rest.Config) (*tenantPodExec, error) {
	if config == nil || config.Timeout <= 0 {
		return nil, fmt.Errorf("bounded validated tenant configuration is required for HBN reads")
	}
	origin, err := url.Parse(config.Host)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return nil, fmt.Errorf("tenant HBN reads require the validated HTTPS origin")
	}
	bound := rest.CopyConfig(config)
	bound.CAData = bytes.Clone(config.CAData)
	bound.CertData = bytes.Clone(config.CertData)
	bound.KeyData = bytes.Clone(config.KeyData)
	bound.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	transport, upgrader, err := spdy.RoundTripperFor(bound)
	if err != nil {
		return nil, fmt.Errorf("cannot construct tenant HBN read transport")
	}
	return &tenantPodExec{origin: origin, transport: transport, upgrader: upgrader, timeout: min(bound.Timeout, 2*time.Minute)}, nil
}

func (e *tenantPodExec) read(ctx context.Context, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
	if e == nil || e.origin == nil || e.transport == nil || e.upgrader == nil || ctx == nil || e.timeout <= 0 || e.timeout > 2*time.Minute {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_UNAVAILABLE")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_DEADLINE_REQUIRED")
	}
	endpoint, err := tenantHBNCommandURL(e.origin, namespace, pod, command, prefix)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_CANCELED")
	}
	executor, err := remotecommand.NewSPDYExecutorRejectRedirects(e.transport, e.upgrader, http.MethodPost, endpoint)
	if err != nil {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_UNAVAILABLE")
	}
	execCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	logger := log.FromContext(ctx).WithName("dpf-hbn-runtime")
	logger.V(1).Info("Reading selected HBN pod runtime", "namespace", namespace, "pod", pod, "command", int(command))
	output, err := captureTenantHBNCommand(execCtx, cancel, executor)
	if err != nil {
		logger.V(1).Info("HBN runtime read did not complete", "reason", err.Error())
	}
	return output, err
}

func tenantHBNCommandURL(origin *url.URL, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) (*url.URL, error) {
	if origin == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(pod)) != 0 {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_INPUT_INVALID")
	}
	var argv []string
	switch command {
	case hbnReadGateway:
		argv = []string{"/usr/bin/nv", "show", "interface", "pf0hpf_if", "-o", "json"}
	case hbnReadBGPSummary:
		argv = []string{"/usr/bin/vtysh", "-c", "show bgp vrf default ipv4 unicast summary json"}
	case hbnReadRemoteRoute:
		if !prefix.IsValid() || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() < 1 || prefix.Bits() > 30 {
			return nil, fmt.Errorf("TENANT_HBN_EXEC_INPUT_INVALID")
		}
		argv = []string{"/usr/bin/vtysh", "-c", "show ip route " + prefix.String() + " json"}
	default:
		return nil, fmt.Errorf("TENANT_HBN_EXEC_INPUT_INVALID")
	}
	if command != hbnReadRemoteRoute && prefix.IsValid() {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_INPUT_INVALID")
	}
	endpoint := *origin
	endpoint.Path = "/api/v1/namespaces/" + namespace + "/pods/" + pod + "/exec"
	query := url.Values{"container": {"doca-hbn"}, "stdout": {"true"}, "stderr": {"true"}, "command": argv}
	endpoint.RawQuery = query.Encode()
	return &endpoint, nil
}

// Both protocol streams can write concurrently and may finish while cancellation
// unwinds. Keep their capture bounded and synchronized; overflow cancels the
// connection even if the streaming library only logs the writer error.
type tenantCommandBuffer struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
	cancel   context.CancelFunc
}

func (b *tenantCommandBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(data) > tenantCommandOutputLimit-len(b.data) {
		b.overflow = true
		b.cancel()
		return 0, fmt.Errorf("TENANT_HBN_EXEC_OUTPUT_LIMIT")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *tenantCommandBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data), b.overflow
}

func captureTenantHBNCommand(ctx context.Context, cancel context.CancelFunc, executor remotecommand.Executor) ([]byte, error) {
	stdout := &tenantCommandBuffer{cancel: cancel}
	stderr := &tenantCommandBuffer{cancel: cancel}
	err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
	output, outputOverflow := stdout.snapshot()
	diagnostic, diagnosticOverflow := stderr.snapshot()
	if outputOverflow || diagnosticOverflow {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_OUTPUT_LIMIT")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_CANCELED")
	}
	if err != nil {
		switch {
		case apierrors.IsForbidden(err):
			return nil, fmt.Errorf("TENANT_HBN_EXEC_FORBIDDEN")
		case apierrors.IsUnauthorized(err):
			return nil, fmt.Errorf("TENANT_HBN_EXEC_UNAUTHORIZED")
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("TENANT_HBN_EXEC_POD_UNAVAILABLE")
		}
		var exit utilexec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitStatus() {
			case 127:
				return nil, fmt.Errorf("TENANT_HBN_EXEC_TOOL_MISSING")
			case 126:
				return nil, fmt.Errorf("TENANT_HBN_EXEC_TOOL_NOT_EXECUTABLE")
			}
		}
		return nil, fmt.Errorf("TENANT_HBN_EXEC_FAILED")
	}
	if len(diagnostic) != 0 || len(bytes.TrimSpace(output)) == 0 {
		return nil, fmt.Errorf("TENANT_HBN_EXEC_RESPONSE_INVALID")
	}
	return output, nil
}
