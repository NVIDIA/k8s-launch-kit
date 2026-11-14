// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/portforward"
	streamhttp "k8s.io/streaming/pkg/httpstream"
	streamspdy "k8s.io/streaming/pkg/httpstream/spdy"
)

func TestRegistryHTTPClientUsesLoopbackSNIAndStrictCA(t *testing.T) {
	caPEM, certificate := testRegistryCertificate(t, "bfb-registry.test.svc")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var gotSNI, gotHost string
	server := &http.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotSNI, gotHost = r.TLS.ServerName, r.Host
			http.NotFound(w, r)
		}),
	}
	go func() { _ = server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	defer server.Close()

	client, transport, err := newRegistryHTTPClient("test", caPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || transport.TLSClientConfig.ServerName != "bfb-registry.test.svc" {
		t.Fatal("registry transport did not disable proxy or pin the service SNI")
	}
	requestURL := fmt.Sprintf("https://127.0.0.1:%d/probe", listener.Addr().(*net.TCPAddr).Port)
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "bfb-registry.test.svc"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trusted loopback request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
		t.Fatalf("unexpected TLS response: status=%d tls=%v", resp.StatusCode, resp.TLS != nil)
	}
	if gotSNI != "bfb-registry.test.svc" || gotHost != "bfb-registry.test.svc" {
		t.Fatalf("registry identity mismatch: SNI=%q Host=%q", gotSNI, gotHost)
	}

	wrongCA, _ := testRegistryCertificate(t, "bfb-registry.test.svc")
	wrongClient, wrongTransport, err := newRegistryHTTPClient("test", wrongCA)
	if err != nil {
		t.Fatal(err)
	}
	defer wrongTransport.CloseIdleConnections()
	if _, err := wrongClient.Get(requestURL); err == nil {
		t.Fatal("TLS accepted an unrelated CA/name")
	}
}

func TestRegistryHTTPClientRejectsInvalidCAAndNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, namespace string
		ca              []byte
	}{
		{name: "invalid namespace", namespace: "ns/escape", ca: []byte("cert")},
		{name: "invalid CA", namespace: "test", ca: []byte("not a certificate")},
		{name: "oversized CA", namespace: "test", ca: make([]byte, registryMaxCABytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := newRegistryHTTPClient(tc.namespace, tc.ca); err == nil {
				t.Fatal("invalid tunnel input accepted")
			}
		})
	}
}

func TestRegistryHTTPClientDoesNotFollowRedirects(t *testing.T) {
	caPEM, certificate := testRegistryCertificate(t, "bfb-registry.test.svc")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var destinationCalls atomic.Int32
	server := &http.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				http.Redirect(w, r, "/destination", http.StatusFound)
				return
			}
			destinationCalls.Add(1)
		}),
	}
	go func() { _ = server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	defer server.Close()
	client, transport, err := newRegistryHTTPClient("test", caPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	requestURL := fmt.Sprintf("https://127.0.0.1:%d/redirect", listener.Addr().(*net.TCPAddr).Port)
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || destinationCalls.Load() != 0 {
		t.Fatalf("redirect was followed: status=%d destinationCalls=%d", resp.StatusCode, destinationCalls.Load())
	}
}

func TestContextualSPDYDialerCancelsHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &blockingRoundTripper{started: make(chan struct{})}
	client := &http.Client{Transport: transport}
	requestURL, _ := url.Parse("https://management.test/api/v1/namespaces/test/pods/bfb-registry/portforward")
	dialer := &contextualSPDYDialer{ctx: ctx, upgrader: testSPDYUpgrader{}, client: client, url: requestURL}
	result := make(chan error, 1)
	go func() {
		_, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		result <- err
	}()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("SPDY handshake did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SPDY handshake did not stop on context cancellation")
	}
}

func TestContextualSPDYDialerClosesConnectionWhenUpgradeHeadersStall(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestReceived := make(chan struct{})
	serverClosed := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			close(serverClosed)
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		close(requestReceived)
		var one [1]byte
		_, _ = conn.Read(one[:])
		close(serverClosed)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tlsConfig := &tls.Config{}
	dialTransport := contextBoundDialTransport(tlsConfig)
	upgrader, err := streamspdy.NewRoundTripperWithConfig(streamspdy.RoundTripperConfig{UpgradeTransport: dialTransport})
	if err != nil {
		t.Fatal(err)
	}
	requestURL, _ := url.Parse(fmt.Sprintf("http://%s/portforward", listener.Addr()))
	dialer := &contextualSPDYDialer{ctx: ctx, upgrader: upgrader, client: &http.Client{Transport: upgrader}, url: requestURL}
	result := make(chan error, 1)
	go func() {
		_, _, dialErr := dialer.Dial(portforward.PortForwardProtocolV1Name)
		result <- dialErr
	}()
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("SPDY request did not reach the stalled local server")
	}
	cancel()
	select {
	case dialErr := <-result:
		if !errors.Is(dialErr, context.Canceled) {
			t.Fatalf("stalled handshake cancellation = %v", dialErr)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled upgrade response ignored context cancellation")
	}
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not close the underlying connection")
	}
}

func TestContextualSPDYDialerKeepsSuccessfulUpgradeConnectionOpen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverClosed := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			close(serverClosed)
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\n%s: %s\r\n\r\n", streamhttp.HeaderProtocolVersion, portforward.PortForwardProtocolV1Name)
		// Drain the complete SPDY shutdown frame before closing the peer. A
		// one-byte read races the client's GOAWAY write and can reset an otherwise
		// healthy upgraded connection while connection.Close is still flushing.
		_, _ = io.Copy(io.Discard, conn)
		close(serverClosed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	upgrader, err := streamspdy.NewRoundTripperWithConfig(streamspdy.RoundTripperConfig{UpgradeTransport: contextBoundDialTransport(&tls.Config{})})
	if err != nil {
		t.Fatal(err)
	}
	requestURL, _ := url.Parse(fmt.Sprintf("http://%s/portforward", listener.Addr()))
	dialer := &contextualSPDYDialer{ctx: ctx, upgrader: upgrader, client: &http.Client{Transport: upgrader}, url: requestURL}
	connection, protocol, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != portforward.PortForwardProtocolV1Name {
		t.Fatalf("negotiated protocol = %q", protocol)
	}
	select {
	case <-connection.CloseChan():
		t.Fatal("successful upgrade connection closed with the HTTP response body")
	case <-time.After(50 * time.Millisecond):
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("closing the SPDY connection did not close the socket")
	}
}

func TestStartRegistryTunnelBindsLoopbackAndClosesIdempotently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	forwardConn := newTestSPDYConnection()
	client := &http.Client{}
	transport := &http.Transport{}
	tunnel, err := startRegistryTunnel(ctx, cancel, "test", staticSPDYDialer{conn: forwardConn, protocol: portforward.PortForwardProtocolV1Name}, client, transport)
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.port == 0 {
		t.Fatal("port-forward did not select an ephemeral port")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", tunnel.port)
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		_ = listener.Close()
		t.Fatal("forwarder did not retain its loopback listener")
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("close tunnel: %v", err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	listener, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("loopback listener remained after cleanup: %v", err)
	}
	_ = listener.Close()
}

func TestRegistryTunnelCloseReportsUnexpectedForwarderLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection := newTestSPDYConnection()
	tunnel, err := startRegistryTunnel(ctx, cancel, "test", staticSPDYDialer{conn: connection, protocol: portforward.PortForwardProtocolV1Name}, &http.Client{}, &http.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	connection.loseRemote()
	select {
	case <-connection.localClosed:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not close after remote connection loss")
	}
	if err := tunnel.Close(); err == nil {
		t.Fatal("unexpected port-forward loss was reported as clean")
	}
}

func TestStartRegistryTunnelCleansUpHandshakeFailureAndCancellation(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := startRegistryTunnel(ctx, cancel, "test", failingSPDYDialer{}, &http.Client{}, &http.Transport{})
		if err == nil {
			t.Fatal("failed handshake returned a tunnel")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ctx, deadlineCancel := context.WithTimeout(ctx, 2*time.Second)
		defer deadlineCancel()
		dialer := blockingSPDYDialer{ctx: ctx, started: make(chan struct{})}
		result := make(chan error, 1)
		go func() {
			_, err := startRegistryTunnel(ctx, deadlineCancel, "test", dialer, &http.Client{}, &http.Transport{})
			result <- err
		}()
		select {
		case <-dialer.started:
		case <-time.After(time.Second):
			t.Fatal("forwarder did not begin handshake")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("startup cancellation = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("startup did not clean up after cancellation")
		}
	})
}

func TestRegistryTunnelCloseReturnsCleanupFailureOnce(t *testing.T) {
	want := errors.New("cleanup fixture")
	var calls int
	tunnel := &registryTunnel{closeFn: func() error { calls++; return want }}
	if err := tunnel.Close(); !errors.Is(err, want) {
		t.Fatalf("first close = %v", err)
	}
	if err := tunnel.Close(); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("second close = %v calls=%d", err, calls)
	}
}

func TestStartupCancellationPreservesTunnelCleanupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want := errors.New("close fixture")
	tunnel := &registryTunnel{closeFn: func() error { return want }}
	err := closeTunnelAfterStartupCancellation(ctx.Err(), tunnel)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, want) {
		t.Fatalf("joined startup cancellation/cleanup = %v", err)
	}
}

func testRegistryCertificate(t *testing.T, dnsName string) ([]byte, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificate
}

type blockingRoundTripper struct{ started chan struct{} }

func (r *blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	close(r.started)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type testSPDYUpgrader struct{}

func (testSPDYUpgrader) NewConnection(*http.Response) (streamhttp.Connection, error) {
	return nil, errors.New("unused test upgrader")
}

type failingSPDYDialer struct{}

func (failingSPDYDialer) Dial(...string) (streamhttp.Connection, string, error) {
	return nil, "", errors.New("dial fixture")
}

type blockingSPDYDialer struct {
	ctx     context.Context
	started chan struct{}
}

func (d blockingSPDYDialer) Dial(...string) (streamhttp.Connection, string, error) {
	close(d.started)
	<-d.ctx.Done()
	return nil, "", d.ctx.Err()
}

type staticSPDYDialer struct {
	conn     streamhttp.Connection
	protocol string
}

func (d staticSPDYDialer) Dial(...string) (streamhttp.Connection, string, error) {
	return d.conn, d.protocol, nil
}

type testSPDYConnection struct {
	remoteClosed chan bool
	localClosed  chan struct{}
	closeOnce    sync.Once
	remoteOnce   sync.Once
}

func newTestSPDYConnection() *testSPDYConnection {
	return &testSPDYConnection{remoteClosed: make(chan bool), localClosed: make(chan struct{})}
}

func (c *testSPDYConnection) CreateStream(http.Header) (streamhttp.Stream, error) {
	return nil, errors.New("unused stream")
}

func (c *testSPDYConnection) Close() error {
	c.closeOnce.Do(func() { close(c.localClosed) })
	c.remoteOnce.Do(func() { close(c.remoteClosed) })
	return nil
}

func (c *testSPDYConnection) CloseChan() <-chan bool { return c.remoteClosed }

func (c *testSPDYConnection) loseRemote() { c.remoteOnce.Do(func() { close(c.remoteClosed) }) }

func (*testSPDYConnection) SetIdleTimeout(time.Duration) {}

func (*testSPDYConnection) RemoveStreams(...streamhttp.Stream) {}

var _ streamhttp.Connection = (*testSPDYConnection)(nil)
var _ responseUpgrader = testSPDYUpgrader{}
