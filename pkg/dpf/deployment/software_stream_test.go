// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyRegistryStream(t *testing.T) {
	data := "selected staged software"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		limit    int64
		digest   string
		wantPass bool
	}{
		{name: "matching stored bytes", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "identity" {
				t.Error("representation was not requested explicitly")
			}
			_, _ = io.WriteString(w, data)
		}, limit: int64(len(data)), digest: digest, wantPass: true},
		{name: "wrong digest", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "other software") }, limit: 100, digest: digest},
		{name: "too large content length", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, data) }, limit: 2, digest: digest},
		{name: "too large chunked body", handler: func(w http.ResponseWriter, _ *http.Request) { w.(http.Flusher).Flush(); _, _ = io.WriteString(w, data) }, limit: 2, digest: digest},
		{name: "empty chunked body", handler: func(w http.ResponseWriter, _ *http.Request) { w.(http.Flusher).Flush() }, limit: 100, digest: fmt.Sprintf("%x", sha256.Sum256(nil))},
		{name: "truncated body", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, data)
		}, limit: 100, digest: digest},
		{name: "compressed response", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, data)
		}, limit: 100, digest: digest},
		{name: "partial content", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, data)
		}, limit: 100, digest: digest},
		{name: "server error", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "private server detail", http.StatusInternalServerError)
		}, limit: 100, digest: digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tc.handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			n, err := verifyRegistryStream(ctx, server.Client(), server.URL+"/bfb/selected.iso", tc.digest, tc.limit)
			if tc.wantPass {
				if err != nil || n != int64(len(data)) {
					t.Fatalf("verification = %d, %v", n, err)
				}
			} else if err == nil || n != 0 {
				t.Fatalf("accepted failed verification: %d, %v", n, err)
			}
			if err != nil && (strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private server detail")) {
				t.Fatal("unsafe diagnostic")
			}
		})
	}
}

func TestRegistryStreamDoesNotFollowRedirectOrMutateClient(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		_, _ = io.WriteString(w, "data")
	}))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	client := source.Client()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := verifyRegistryStream(ctx, client, source.URL, strings.Repeat("a", 64), 100)
	if err == nil || destinationCalls.Load() != 0 || client.CheckRedirect != nil {
		t.Fatalf("redirect boundary failed: %v", err)
	}
}

func TestRegistryStreamCancellationAndInputFailures(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { calls.Add(1); close(started); <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, input := range []struct {
		url, digest string
		limit       int64
	}{
		{server.URL, "bad-digest", 100},
		{server.URL + "?credential=private", strings.Repeat("a", 64), 100},
		{server.URL, strings.Repeat("a", 64), 0},
		{server.URL, strings.Repeat("A", 64), 100},
	} {
		if _, err := verifyRegistryStream(ctx, server.Client(), input.url, input.digest, input.limit); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input made a network request")
	}
	if _, err := verifyRegistryStream(context.Background(), server.Client(), server.URL, strings.Repeat("a", 64), 100); err == nil {
		t.Fatal("unbounded context accepted")
	}
	done := make(chan error, 1)
	go func() {
		_, err := verifyRegistryStream(ctx, server.Client(), server.URL, strings.Repeat("a", 64), 100)
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
}
