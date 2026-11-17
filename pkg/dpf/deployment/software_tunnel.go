// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	streamhttp "k8s.io/streaming/pkg/httpstream"
	streamspdy "k8s.io/streaming/pkg/httpstream/spdy"
)

const (
	registryPod          = "bfb-registry"
	registryRemotePort   = 8443
	registryMaxCABytes   = 1 << 20
	registryForwardClose = 3 * time.Second
)

// registryTunnel owns the local port-forward listener and HTTPS transport.
// closeFn is injectable for lifecycle tests; production closes both resources.
type registryTunnel struct {
	port   int
	client *http.Client

	closeFn   func() error
	closeOnce sync.Once
	closeErr  error
}

func (t *registryTunnel) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.closeFn != nil {
			t.closeErr = t.closeFn()
		}
	})
	return t.closeErr
}

// openRegistryTunnel connects only to the existing registry Pod's port-forward
// endpoint. restConfig must be the caller's already-bound management snapshot.
func openRegistryTunnel(ctx context.Context, restConfig *rest.Config, namespace string, caPEM []byte) (*registryTunnel, error) {
	if ctx == nil || restConfig == nil || !validRegistryNamespace(namespace) || len(caPEM) == 0 || len(caPEM) > registryMaxCABytes {
		return nil, fmt.Errorf("registry tunnel requires a bounded context, bound REST config, namespace and public CA bundle")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("registry tunnel requires a context deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	client, transport, err := newRegistryHTTPClient(namespace, caPEM)
	if err != nil {
		return nil, err
	}
	config := rest.CopyConfig(restConfig)
	// The tunnel must be reached directly through the bound Kubernetes API
	// endpoint; environment and kubeconfig proxies are not part of this path.
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("cannot construct the bound registry port-forward client")
	}
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("cannot construct the bound registry TLS transport")
	}
	if tlsConfig == nil {
		// Streaming SPDY treats a nil TLS config in UpgradeTransport as an
		// explicit insecure-verification request; an empty config retains the
		// normal system-root verification behavior instead.
		tlsConfig = &tls.Config{}
	}
	portForwardURL := coreClient.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(registryPod).SubResource("portforward").URL()
	forwardCtx, cancelForward := context.WithCancel(ctx)
	dialTransport := contextBoundDialTransport(tlsConfig)
	upgradeTransport, err := streamspdy.NewRoundTripperWithConfig(streamspdy.RoundTripperConfig{
		UpgradeTransport: dialTransport,
		PingPeriod:       5 * time.Second,
	})
	if err != nil {
		cancelForward()
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("cannot construct the context-bound registry SPDY transport")
	}
	roundTripper, err := rest.HTTPWrappersForConfig(config, upgradeTransport)
	if err != nil {
		cancelForward()
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("cannot construct the bound registry REST authentication transport")
	}
	spdyClient := &http.Client{Transport: roundTripper,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	dialer := &contextualSPDYDialer{ctx: forwardCtx, upgrader: upgradeTransport, client: spdyClient, url: portForwardURL}
	tunnel, err := startRegistryTunnel(forwardCtx, cancelForward, namespace, dialer, client, transport)
	if err != nil {
		cancelForward()
		transport.CloseIdleConnections()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, closeTunnelAfterStartupCancellation(err, tunnel)
	}
	return tunnel, nil
}

func newRegistryHTTPClient(namespace string, caPEM []byte) (*http.Client, *http.Transport, error) {
	if !validRegistryNamespace(namespace) || len(caPEM) == 0 || len(caPEM) > registryMaxCABytes {
		return nil, nil, fmt.Errorf("registry HTTPS requires a valid namespace and bounded public CA bundle")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("public registry CA bundle is invalid")
	}
	serviceName := "bfb-registry." + namespace + ".svc"
	transport := &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  true,
		DisableCompression: true,
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			ServerName: serviceName,
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, transport, nil
}

type contextualSPDYDialer struct {
	ctx      context.Context
	upgrader responseUpgrader
	client   *http.Client
	url      *url.URL
}

type responseUpgrader interface {
	NewConnection(*http.Response) (streamhttp.Connection, error)
}

func (d *contextualSPDYDialer) Dial(protocols ...string) (streamhttp.Connection, string, error) {
	if d == nil || d.ctx == nil || d.upgrader == nil || d.client == nil || d.url == nil {
		return nil, "", fmt.Errorf("registry SPDY dialer is incomplete")
	}
	if err := d.ctx.Err(); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, d.url.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("cannot construct registry port-forward request")
	}
	for _, protocol := range protocols {
		req.Header.Add(streamhttp.HeaderProtocolVersion, protocol)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		if ctxErr := d.ctx.Err(); ctxErr != nil {
			return nil, "", ctxErr
		}
		return nil, "", fmt.Errorf("registry port-forward negotiation failed")
	}
	connection, err := d.upgrader.NewConnection(resp)
	if err != nil {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("registry port-forward negotiation failed")
	}
	// A successful 101 response transfers the socket to the SPDY connection.
	// Its lifetime is managed by the port-forwarder, not this HTTP response.
	return connection, resp.Header.Get(streamhttp.HeaderProtocolVersion), nil
}

type forwardResult struct {
	done chan struct{}
	err  error
}

func startRegistryTunnel(ctx context.Context, cancel context.CancelFunc, namespace string, dialer streamhttp.Dialer, client *http.Client, transport *http.Transport) (*registryTunnel, error) {
	if ctx == nil || cancel == nil || !validRegistryNamespace(namespace) || dialer == nil || client == nil || transport == nil {
		return nil, fmt.Errorf("registry tunnel startup inputs are incomplete")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("registry tunnel requires a context deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddressesForStreamingWithContext(ctx, dialer, []string{"127.0.0.1"}, []string{"0:8443"}, ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot prepare registry port-forward listener")
	}
	completion := &forwardResult{done: make(chan struct{})}
	go func() {
		completion.err = forwarder.ForwardPorts()
		close(completion.done)
	}()
	stopForward := cancel
	cleanupUnstarted := func() bool {
		stopForward()
		select {
		case <-completion.done:
			return true
		case <-time.After(registryForwardClose):
			return false
		}
	}

	select {
	case <-ready:
		ports, err := forwarder.GetPorts()
		if err != nil || len(ports) != 1 || ports[0].Remote != registryRemotePort || ports[0].Local == 0 {
			if !cleanupUnstarted() {
				transport.CloseIdleConnections()
				return nil, fmt.Errorf("registry port-forward cleanup timed out")
			}
			return nil, fmt.Errorf("registry port-forward did not bind the requested local port")
		}
		closed := make(chan struct{})
		tunnel := &registryTunnel{port: int(ports[0].Local), client: client}
		tunnel.closeFn = func() error {
			defer close(closed)
			priorContextErr := ctx.Err()
			stopForward()
			select {
			case <-completion.done:
				transport.CloseIdleConnections()
				if completion.err != nil && priorContextErr == nil {
					return fmt.Errorf("registry port-forward stopped unexpectedly")
				}
				return nil
			case <-time.After(registryForwardClose):
				transport.CloseIdleConnections()
				return fmt.Errorf("registry port-forward cleanup timed out")
			}
		}
		// Cancellation closes the local listener even if the caller is delayed
		// before its deferred Close runs.
		go func() {
			select {
			case <-ctx.Done():
				_ = tunnel.Close()
			case <-closed:
			}
		}()
		return tunnel, nil
	case <-ctx.Done():
		if !cleanupUnstarted() {
			transport.CloseIdleConnections()
			return nil, errors.Join(ctx.Err(), fmt.Errorf("registry port-forward cleanup timed out"))
		}
		transport.CloseIdleConnections()
		return nil, ctx.Err()
	case <-completion.done:
		stopForward()
		transport.CloseIdleConnections()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("registry port-forward ended before binding")
	}
}

func closeTunnelAfterStartupCancellation(ctxErr error, tunnel *registryTunnel) error {
	return errors.Join(ctxErr, tunnel.Close())
}

func validRegistryNamespace(namespace string) bool {
	return len(validation.IsDNS1123Label(namespace)) == 0
}

var _ streamhttp.Dialer = (*contextualSPDYDialer)(nil)

type contextBoundConn struct {
	net.Conn
	stopCancel func() bool
	closeOnce  sync.Once
	closeErr   error
}

func (c *contextBoundConn) Close() error {
	c.closeOnce.Do(func() {
		if c.stopCancel != nil {
			c.stopCancel()
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func contextBoundDialTransport(tlsConfig *tls.Config) *http.Transport {
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	return &http.Transport{
		Proxy:           nil,
		TLSClientConfig: tlsConfig,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
			return &contextBoundConn{Conn: conn, stopCancel: stopCancel}, nil
		},
	}
}
