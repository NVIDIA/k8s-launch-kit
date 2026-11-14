// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
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

func TestChartDownloadAcceptsHTTPSObjectSignatureRedirect(t *testing.T) {
	var finalCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chart.tgz" {
			http.Redirect(w, r, "/object?Signature=private-signature", http.StatusFound)
			return
		}
		if r.URL.Query().Get("Signature") != "private-signature" {
			http.Error(w, "signature missing", http.StatusForbidden)
			return
		}
		if r.Header.Get("Referer") != "" {
			http.Error(w, "unexpected Referer", 400)
			return
		}
		finalCalls.Add(1)
		fmt.Fprint(w, "chart bytes")
	}))
	defer server.Close()
	data, err := downloadChart(context.Background(), server.URL+"/chart.tgz", server.Client().Transport)
	if err != nil || string(data) != "chart bytes" || finalCalls.Load() != 1 {
		t.Fatalf("signed redirect failed: %s, %v", data, err)
	}
	if _, err := downloadChart(context.Background(), server.URL+"/object?Signature=private-signature", server.Client().Transport); err == nil {
		t.Fatal("signed input coordinate accepted")
	}
}

func TestChartDownloadDoesNotReferSignedURLOnNextRedirect(t *testing.T) {
	var finalCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chart.tgz":
			http.Redirect(w, r, "/object?Signature=private-signature", http.StatusFound)
		case "/object":
			if r.URL.Query().Get("Signature") != "private-signature" {
				http.Error(w, "signature missing", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			if r.Header.Get("Referer") != "" {
				http.Error(w, "unexpected Referer", http.StatusBadRequest)
				return
			}
			finalCalls.Add(1)
			fmt.Fprint(w, "chart bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	data, err := downloadChart(context.Background(), server.URL+"/chart.tgz", server.Client().Transport)
	if err != nil || string(data) != "chart bytes" || finalCalls.Load() != 1 {
		t.Fatalf("signed multi-hop redirect failed: %s, %v", data, err)
	}
}

func TestChartDownloadRejectsUnsafeRedirectWithoutExposingURL(t *testing.T) {
	for _, target := range []string{"http://example.invalid/object?Signature=private-signature", "https://user:private-signature@example.invalid/chart", "https://example.invalid/chart#private-signature"} {
		t.Run(target[:5], func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) }))
			defer server.Close()
			_, err := downloadChart(context.Background(), server.URL, server.Client().Transport)
			if err == nil || !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), "private-signature") {
				t.Fatalf("unsafe redirect error not redacted: %v", err)
			}
		})
	}
}

func TestChartDownloadRedirectLoopAndTransportErrorAreRedacted(t *testing.T) {
	for _, mode := range []string{"loop", "transport"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/object?Signature=private-signature", http.StatusFound)
			}))
			defer server.Close()
			transport := http.RoundTripper(server.Client().Transport)
			if mode == "transport" {
				transport = chartTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.RawQuery != "" {
						return nil, errors.New("private-signature reflected in failure")
					}
					return server.Client().Transport.RoundTrip(r)
				})
			}
			_, err := downloadChart(context.Background(), server.URL, transport)
			if err == nil || strings.Contains(err.Error(), "private-signature") {
				t.Fatalf("redirect failure exposed query: %v", err)
			}
			if mode == "loop" && !strings.Contains(err.Error(), "limit") {
				t.Fatalf("loop not bounded: %v", err)
			}
		})
	}
}

func TestChartDownloadCancellationAndHTTPError(t *testing.T) {
	for _, mode := range []string{"cancel", "http"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "cancel" {
					<-r.Context().Done()
					return
				}
				http.Error(w, "private-signature", http.StatusForbidden)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := downloadChart(ctx, server.URL, server.Client().Transport)
			if mode == "cancel" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("context not preserved: %v", err)
			}
			if mode == "http" && (err == nil || err.Error() != "chart server returned HTTP 403") {
				t.Fatalf("HTTP failure not safely reported: %v", err)
			}
		})
	}
}

type chartTestTransport func(*http.Request) (*http.Response, error)

func (f chartTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChartDownloadPreservesCancellationWhenTransportReturnsNormally(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := chartTestTransport(func(*http.Request) (*http.Response, error) {
				body := io.NopCloser(chartTestReader(func([]byte) (int, error) {
					cancel()
					return 0, io.EOF
				}))
				status := http.StatusOK
				if phase == "headers" {
					cancel()
					status = http.StatusForbidden
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})
			data, err := downloadChart(ctx, "https://charts.example.test/chart.tgz", transport)
			if !errors.Is(err, context.Canceled) || data != nil {
				t.Fatalf("cancelled %s returned data=%q error=%v", phase, data, err)
			}
		})
	}
}

type chartTestReader func([]byte) (int, error)

func (r chartTestReader) Read(p []byte) (int, error) { return r(p) }
