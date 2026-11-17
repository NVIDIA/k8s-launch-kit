// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package artifactcheck verifies configured software artifact bytes without
// retaining them in memory or writing them to disk.
package artifactcheck

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const (
	MaxArtifactBytes int64 = 16 << 30
	MaxTotalBytes    int64 = 32 << 30
	MaxRedirects           = 5
	MaxDuration            = 30 * time.Minute
)

// Artifact identifies one immutable source. Subject is safe to include in a
// report; URL must not be logged or returned in errors.
type Artifact struct {
	Subject string
	URL     string
	SHA256  string
}

// Result is a URL-free summary suitable for discovery inventory.
type Result struct {
	Subject    string
	State      string
	ReasonCode string
	Message    string
	Bytes      int64
}

// Verifier permits transport substitution in tests. Zero values use the
// standard TLS-verifying HTTP transport and the package's fixed safety bounds.
type Verifier struct {
	Transport http.RoundTripper
}

// limits are resource ceilings, not artifact-format or hardware requirements.
type limits struct {
	MaxArtifactBytes int64
	MaxTotalBytes    int64
	MaxDuration      time.Duration
}

func (v Verifier) Verify(ctx context.Context, artifacts []Artifact) []Result {
	return v.verify(ctx, artifacts, limits{MaxArtifactBytes: MaxArtifactBytes, MaxTotalBytes: MaxTotalBytes, MaxDuration: MaxDuration})
}

func (v Verifier) verify(ctx context.Context, artifacts []Artifact, limits limits) []Result {
	results := make([]Result, len(artifacts))
	if len(artifacts) == 0 {
		return results
	}
	if ctx == nil {
		for i, artifact := range artifacts {
			results[i] = unknown(artifact.Subject, "ARTIFACT_REQUEST_INVALID", "Artifact verification requires a context.", 0)
		}
		return results
	}
	if limits.MaxArtifactBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxDuration <= 0 {
		for i, artifact := range artifacts {
			results[i] = unknown(artifact.Subject, "ARTIFACT_LIMITS_INVALID", "Artifact verification limits are invalid.", 0)
		}
		return results
	}
	verifyCtx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	transport := v.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return errRedirectLimit
		}
		if err := validateURL(req.URL, true); err != nil {
			return errUnsafeRedirect
		}
		// Signed redirect URLs must not be copied into a subsequent Referer.
		req.Header.Del("Referer")
		return nil
	}}

	type key struct{ url, digest string }
	seen := make(map[key]Result, len(artifacts))
	var total int64
	output := ui.FromContext(ctx)
	for i, artifact := range artifacts {
		k := key{artifact.URL, artifact.SHA256}
		if previous, ok := seen[k]; ok {
			previous.Subject = artifact.Subject
			results[i] = previous
			continue
		}
		if err := verifyCtx.Err(); err != nil {
			results[i] = unknown(artifact.Subject, "ARTIFACT_CHECK_CANCELLED", "Artifact verification did not complete before the discovery deadline.", 0)
			seen[k] = results[i]
			continue
		}
		output.Info("[RUNNING] Artifact %s: reading HTTPS source and verifying SHA-256", artifact.Subject)
		result, consumed := verifyOne(verifyCtx, client, artifact, limits.MaxArtifactBytes, limits.MaxTotalBytes-total)
		status := "PASS"
		if result.State != "passed" {
			status = "UNAVAILABLE"
			if result.State == "failed" {
				status = "FAIL"
			}
		}
		output.Info("[%s] Artifact %s: %s (%d bytes read)", status, artifact.Subject, result.Message, result.Bytes)
		total += consumed
		results[i] = result
		seen[k] = result
	}
	return results
}

var (
	errUnsafeRedirect = errors.New("unsafe artifact redirect")
	errRedirectLimit  = errors.New("artifact redirect limit exceeded")
)

func verifyOne(ctx context.Context, client *http.Client, artifact Artifact, artifactLimit, totalRemaining int64) (Result, int64) {
	if !validDigest(artifact.SHA256) {
		return unknown(artifact.Subject, "ARTIFACT_LOCK_INVALID", "The configured artifact digest is invalid.", 0), 0
	}
	limit := artifactLimit
	limitCode := "ARTIFACT_SIZE_LIMIT"
	if totalRemaining < limit {
		limit = totalRemaining
		limitCode = "ARTIFACT_TOTAL_SIZE_LIMIT"
	}
	if limit <= 0 {
		return failed(artifact.Subject, limitCode, "Artifact verification exceeded its configured byte limit.", 0), 0
	}
	parsed, err := url.Parse(artifact.URL)
	if err != nil || validateURL(parsed, false) != nil {
		return unknown(artifact.Subject, "ARTIFACT_URL_INVALID", "The configured artifact source is not a permitted HTTPS URL.", 0), 0
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return unknown(artifact.Subject, "ARTIFACT_REQUEST_INVALID", "The artifact request could not be constructed.", 0), 0
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return unknown(artifact.Subject, "ARTIFACT_CHECK_CANCELLED", "Artifact verification did not complete before the discovery deadline.", 0), 0
		}
		if errors.Is(err, errUnsafeRedirect) {
			return failed(artifact.Subject, "ARTIFACT_REDIRECT_REJECTED", "The artifact server redirected to an unsafe URL.", 0), 0
		}
		if errors.Is(err, errRedirectLimit) {
			return failed(artifact.Subject, "ARTIFACT_REDIRECT_LIMIT", "The artifact server exceeded the redirect limit.", 0), 0
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return unknown(artifact.Subject, "ARTIFACT_SOURCE_TIMEOUT", "The artifact source did not respond before the verification deadline.", 0), 0
		}
		return unknown(artifact.Subject, "ARTIFACT_SOURCE_UNAVAILABLE", "The artifact source could not be read from the discovery executor.", 0), 0
	}
	defer response.Body.Close()
	if err := ctx.Err(); err != nil {
		return unknown(artifact.Subject, "ARTIFACT_CHECK_CANCELLED", "Artifact verification did not complete before the discovery deadline.", 0), 0
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			return unknown(artifact.Subject, "ARTIFACT_SOURCE_HTTP_UNAVAILABLE", "The artifact server is temporarily unavailable.", 0), 0
		}
		return failed(artifact.Subject, "ARTIFACT_HTTP_STATUS", fmt.Sprintf("The artifact server returned HTTP %d.", response.StatusCode), 0), 0
	}
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return failed(artifact.Subject, "ARTIFACT_ENCODING_UNSUPPORTED", "The artifact source returned an unsupported content encoding.", 0), 0
	}
	if response.ContentLength > limit {
		return failed(artifact.Subject, limitCode, "Artifact verification exceeded its configured byte limit.", 0), 0
	}
	h := sha256.New()
	read, err := io.Copy(h, io.LimitReader(response.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return unknown(artifact.Subject, "ARTIFACT_CHECK_CANCELLED", "Artifact verification did not complete before the discovery deadline.", read), read
		}
		return unknown(artifact.Subject, "ARTIFACT_READ_FAILED", "The artifact response ended before it could be verified.", read), read
	}
	if read > limit {
		return failed(artifact.Subject, limitCode, "Artifact verification exceeded its configured byte limit.", limit), limit
	}
	if err := ctx.Err(); err != nil {
		return unknown(artifact.Subject, "ARTIFACT_CHECK_CANCELLED", "Artifact verification did not complete before the discovery deadline.", read), read
	}
	if read == 0 {
		return failed(artifact.Subject, "ARTIFACT_EMPTY", "The artifact source returned an empty object.", 0), 0
	}
	if subtle.ConstantTimeCompare(h.Sum(nil), mustDecodeDigest(artifact.SHA256)) != 1 {
		return failed(artifact.Subject, "ARTIFACT_DIGEST_MISMATCH", "The downloaded artifact bytes do not match the configured SHA-256 digest.", read), read
	}
	return Result{Subject: artifact.Subject, State: "passed", ReasonCode: "ARTIFACT_SOURCE_VERIFIED", Message: "The locked artifact bytes were read and matched from the discovery executor's HTTPS vantage.", Bytes: read}, read
}

func validateURL(u *url.URL, redirected bool) error {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errUnsafeRedirect
	}
	if !redirected && (u.RawQuery != "" || u.ForceQuery) {
		return errUnsafeRedirect
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func mustDecodeDigest(value string) []byte {
	digest, _ := hex.DecodeString(value)
	return digest
}

func failed(subject, code, message string, bytes int64) Result {
	return Result{Subject: subject, State: "failed", ReasonCode: code, Message: message, Bytes: bytes}
}

func unknown(subject, code, message string, bytes int64) Result {
	return Result{Subject: subject, State: "unknown", ReasonCode: code, Message: message, Bytes: bytes}
}
