// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// verifyRegistryStream hashes the bytes served through an already identity-bound
// registry transport. The caller owns TLS trust, the local tunnel, cache-path
// selection and before/after Kubernetes identity checks. A match is a point-in-
// time observation, not a guarantee about a later DPF hardware transfer.
func verifyRegistryStream(ctx context.Context, client *http.Client, requestURL, expectedSHA256 string, maxBytes int64) (int64, error) {
	if ctx == nil || client == nil || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return 0, fmt.Errorf("software verification requires a client, context and bounded positive byte limit")
	}
	if _, ok := ctx.Deadline(); !ok {
		return 0, fmt.Errorf("software verification requires a deadline")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size || expectedSHA256 != strings.ToLower(expectedSHA256) {
		return 0, fmt.Errorf("software verification requires a canonical SHA-256 lock")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.Fragment != "" {
		return 0, fmt.Errorf("software verification requires a query-free HTTPS registry URL")
	}
	// Copy the client rather than changing its owner's redirect policy. Registry
	// redirects must never switch the checked backend or forward authentication.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := boundedClient.Do(req)
	if err != nil {
		return 0, softwareStreamError(ctx, "cannot read staged software from the verified registry")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("registry did not return staged software (HTTP %d)", resp.StatusCode)
	}
	if resp.Uncompressed || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return 0, fmt.Errorf("registry software response must preserve the stored byte representation")
	}
	if resp.ContentLength > maxBytes || resp.ContentLength == 0 {
		return 0, fmt.Errorf("registry software size is empty or exceeds the verification limit")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return 0, softwareStreamError(ctx, "registry software stream ended before verification completed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if count == 0 || count > maxBytes || resp.ContentLength >= 0 && count != resp.ContentLength {
		return 0, fmt.Errorf("registry software stream has an empty, excessive or inconsistent size")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return 0, fmt.Errorf("staged software SHA-256 differs from the retained lock")
	}
	return count, nil
}

func softwareStreamError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// HTTP and reader errors may contain URLs, server bodies or credentials.
	return fmt.Errorf("%s", message)
}
