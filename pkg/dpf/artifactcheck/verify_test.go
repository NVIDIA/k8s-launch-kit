// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package artifactcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestVerifyProgressDoesNotExposeSourceURLs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "image")
	}))
	defer server.Close()
	var output bytes.Buffer
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&output))
	result := testVerifier().Verify(ctx, []Artifact{{Subject: "osISO", URL: server.URL + "/private-path", SHA256: digest("image")}})
	if len(result) != 1 || result[0].State != "passed" {
		t.Fatalf("verification failed: %+v", result)
	}
	for _, expected := range []string{"[RUNNING] Artifact osISO", "[PASS] Artifact osISO", "5 bytes read"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("progress is missing %s", expected)
		}
	}
	if strings.Contains(output.String(), server.URL) || strings.Contains(output.String(), "private-path") {
		t.Fatal("progress exposed a source URL")
	}
}

func TestVerifyRejectsNilContext(t *testing.T) {
	var missing context.Context
	result := testVerifier().Verify(missing, []Artifact{{Subject: "osISO"}})
	if len(result) != 1 || result[0].State != "unknown" || result[0].ReasonCode != "ARTIFACT_REQUEST_INVALID" {
		t.Fatalf("nil context accepted: %+v", result)
	}
}

func TestVerifyStreamsAndChecksLockedBytes(t *testing.T) {
	body := strings.Repeat("image", 4096)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("unexpected request method/encoding: %s %q", r.Method, r.Header.Get("Accept-Encoding"))
		}
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	verifier := testVerifier()
	result := verifier.Verify(context.Background(), []Artifact{{Subject: "osISO", URL: server.URL + "/image", SHA256: digest(body)}})
	if len(result) != 1 || result[0].State != "passed" || result[0].Bytes != int64(len(body)) {
		t.Fatalf("result = %#v", result)
	}
	result = verifier.Verify(context.Background(), []Artifact{{Subject: "osISO", URL: server.URL + "/image", SHA256: digest("wrong")}})
	if result[0].State != "failed" || result[0].ReasonCode != "ARTIFACT_DIGEST_MISMATCH" {
		t.Fatalf("mismatch result = %#v", result)
	}
}

func TestVerifyDeduplicatesIdenticalLocksAndEnforcesByteCaps(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, "0123456789")
	}))
	defer server.Close()
	verifier := testVerifier()
	lock := Artifact{Subject: "osISO", URL: server.URL, SHA256: digest("0123456789")}
	results := verifier.Verify(context.Background(), []Artifact{lock, {Subject: "MT_0000001774", URL: lock.URL, SHA256: lock.SHA256}})
	if len(results) != 2 || results[0].State != "passed" || results[1].State != "passed" || requests.Load() != 1 {
		t.Fatalf("deduplicated results=%#v requests=%d", results, requests.Load())
	}

	results = verifier.verify(context.Background(), []Artifact{lock}, limits{MaxArtifactBytes: 5, MaxTotalBytes: 20, MaxDuration: time.Second})
	if results[0].State != "failed" || results[0].ReasonCode != "ARTIFACT_SIZE_LIMIT" || results[0].Bytes != 0 {
		t.Fatalf("per-artifact limit result = %#v", results[0])
	}

	first := Artifact{Subject: "one", URL: server.URL + "/one", SHA256: digest("0123456789")}
	second := Artifact{Subject: "two", URL: server.URL + "/two", SHA256: digest("0123456789")}
	results = verifier.verify(context.Background(), []Artifact{first, second}, limits{MaxArtifactBytes: 20, MaxTotalBytes: 15, MaxDuration: time.Second})
	if results[0].State != "passed" || results[1].State != "failed" || results[1].ReasonCode != "ARTIFACT_TOTAL_SIZE_LIMIT" {
		t.Fatalf("aggregate limit results = %#v", results)
	}
}

func TestVerifyAcceptsHTTPSSignedRedirectWithoutForwardingReferer(t *testing.T) {
	var targetRequests atomic.Int32
	var referer string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		referer = r.Header.Get("Referer")
		_, _ = fmt.Fprint(w, "payload")
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/signed?token=private", http.StatusFound)
	}))
	defer source.Close()
	result := testVerifier().Verify(context.Background(), []Artifact{{Subject: "osISO", URL: source.URL, SHA256: digest("payload")}})
	if result[0].State != "passed" || targetRequests.Load() != 1 || referer != "" {
		t.Fatalf("result=%#v targetRequests=%d referer=%q", result, targetRequests.Load(), referer)
	}
	if strings.Contains(result[0].Message, "private") || strings.Contains(result[0].Message, "token") {
		t.Fatalf("result leaked redirect URL: %#v", result[0])
	}
}

func TestVerifyRejectsUnsafeRedirectAndReportsNoURL(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "payload") }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/signed?token=private", http.StatusFound)
	}))
	defer source.Close()
	result := testVerifier().Verify(context.Background(), []Artifact{{Subject: "osISO", URL: source.URL, SHA256: digest("payload")}})
	if result[0].State != "failed" || result[0].ReasonCode != "ARTIFACT_REDIRECT_REJECTED" || strings.Contains(result[0].Message, "private") {
		t.Fatalf("unsafe redirect result = %#v", result[0])
	}
}

func TestVerifyCancellationUnblocksResponseAndMarksUnverified(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan []Result, 1)
	go func() {
		resultCh <- testVerifier().Verify(ctx, []Artifact{{Subject: "osISO", URL: server.URL, SHA256: digest("payload")}})
	}()
	<-started
	cancel()
	select {
	case result := <-resultCh:
		if result[0].State != "unknown" || result[0].ReasonCode != "ARTIFACT_CHECK_CANCELLED" {
			t.Fatalf("cancel result = %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verification did not stop after cancellation")
	}
}

func TestVerifyRejectsNonSuccessAndInvalidLocks(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/temporary":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		case "/empty":
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	verifier := testVerifier()
	results := verifier.Verify(context.Background(), []Artifact{
		{Subject: "not-found", URL: server.URL + "/missing", SHA256: digest("x")},
		{Subject: "bad-digest", URL: server.URL, SHA256: "bad"},
		{Subject: "bad-url", URL: "http://example.invalid/x", SHA256: digest("x")},
		{Subject: "temporary", URL: server.URL + "/temporary", SHA256: digest("x")},
		{Subject: "empty", URL: server.URL + "/empty", SHA256: digest("")},
	})
	if results[0].ReasonCode != "ARTIFACT_HTTP_STATUS" || results[1].ReasonCode != "ARTIFACT_LOCK_INVALID" || results[2].ReasonCode != "ARTIFACT_URL_INVALID" || results[3].State != "unknown" || results[3].ReasonCode != "ARTIFACT_SOURCE_HTTP_UNAVAILABLE" || results[4].State != "failed" || results[4].ReasonCode != "ARTIFACT_EMPTY" {
		t.Fatalf("invalid results = %#v", results)
	}
}

func testVerifier() Verifier {
	return Verifier{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- test-only local TLS servers.
}

func digest(value string) string {
	result := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", result)
}
