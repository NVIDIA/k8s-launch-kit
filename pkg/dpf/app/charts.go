// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const maximumGenerationChartBytes = 32 << 20

// chartDownloadError carries only diagnostics constructed by the downloader.
// Underlying HTTP errors, response bodies and signed redirect URLs are excluded.
type chartDownloadError struct {
	code    string
	message string
}

func (e *chartDownloadError) Error() string { return e.message }

// DownloadChart retrieves one public HTTPS coordinate with bounded redirects,
// duration and response size. Returned diagnostics omit server text and signed URLs.
func DownloadChart(ctx context.Context, address string) ([]byte, error) {
	return downloadChart(ctx, address, http.DefaultTransport)
}

// Inputs remain unsigned public coordinates. NGC redirects these coordinates to
// signed HTTPS object URLs; queries are allowed only on those server redirects.
// Integrity is verified against the selected digest before bundle publication.
func downloadChart(ctx context.Context, address string, transport http.RoundTripper) ([]byte, error) {
	validate := func(address string, redirected bool) error {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (!redirected && parsed.RawQuery != "") || parsed.Fragment != "" {
			return &chartDownloadError{code: "URL_INVALID", message: "chart URL requires HTTPS without user information or fragment; input coordinates must not contain a query"}
		}
		return nil
	}
	if err := validate(address, false); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, &chartDownloadError{code: "URL_INVALID", message: "cannot construct chart download request"}
	}
	unsafeRedirect := &chartDownloadError{code: "REDIRECT_REJECTED", message: "chart redirect requires HTTPS without user information or fragment"}
	redirectLimit := &chartDownloadError{code: "REDIRECT_LIMIT", message: "chart redirect limit exceeded"}
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return redirectLimit
		}
		if err := validate(req.URL.String(), true); err != nil {
			return unsafeRedirect
		}
		// Do not propagate a prior signed object URL as a Referer on another hop.
		req.Header.Del("Referer")
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		// net/http wraps failures with the final URL, which can contain a signed
		// credential. Never return that wrapper or arbitrary server text.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, unsafeRedirect) {
			return nil, unsafeRedirect
		}
		if errors.Is(err, redirectLimit) {
			return nil, redirectLimit
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, &chartDownloadError{code: "TIMEOUT", message: "chart download timed out"}
		}
		return nil, &chartDownloadError{code: "TRANSPORT_UNAVAILABLE", message: "chart download failed; verify HTTPS connectivity and CA trust"}
	}
	defer response.Body.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &chartDownloadError{code: "HTTP_ERROR", message: fmt.Sprintf("chart server returned HTTP %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumGenerationChartBytes+1))
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, &chartDownloadError{code: "TIMEOUT", message: "chart download timed out"}
		}
		return nil, &chartDownloadError{code: "READ_ERROR", message: "cannot read complete chart archive"}
	}
	if len(data) == 0 || len(data) > maximumGenerationChartBytes {
		return nil, &chartDownloadError{code: "SIZE_INVALID", message: fmt.Sprintf("chart archive must be between 1 byte and %d bytes", maximumGenerationChartBytes)}
	}
	return data, nil
}
