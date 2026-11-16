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
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type identitySoftwareClient struct {
	*softwareVerificationFake
	cause         error
	failAt, calls int
}

func (c *identitySoftwareClient) VerifyIdentity(context.Context) error {
	c.calls++
	if c.calls == c.failAt {
		return c.cause
	}
	return nil
}

func TestSoftwareIdentityCausePreservationDoesNotRetryRegistryReads(t *testing.T) {
	for _, boundary := range []int{1, 2} {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "drift", "not found", "conflict"} {
			t.Run(fmt.Sprintf("%d/%s", boundary, scenario), func(t *testing.T) {
				desired, current, locks, selected := softwareCacheFixture()
				osData, pldmData := "selected OS image", "selected firmware"
				locks.OSISO.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(osData)))
				pldm := locks.PLDM["mt_testpsid"]
				pldm.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(pldmData)))
				locks.PLDM["mt_testpsid"] = pldm
				var reads atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reads.Add(1)
					data := osData
					if strings.HasPrefix(r.URL.Path, "/bfb/components/") {
						data = pldmData
					}
					_, _ = io.WriteString(w, data)
				}))
				defer server.Close()
				endpoint, _ := url.Parse(server.URL)
				port, _ := strconv.Atoi(endpoint.Port())
				closed := 0
				code, cause := identityFlowCause(scenario)
				client := &identitySoftwareClient{softwareVerificationFake: &softwareVerificationFake{current: current, managerImage: "synthetic-release-image", tunnel: &registryTunnel{port: port, client: server.Client(), closeFn: func() error { closed++; return nil }}}, cause: cause, failAt: boundary}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				receipt, err := verifyStagedSoftware(ctx, client, softwareVerifierFunc(func(*unstructured.Unstructured, *unstructured.Unstructured) error { return nil }), desired, current, locks, selected, "operator", "synthetic-release-image", 1024)
				var diagnostic *ReconciliationError
				if err == nil || !errors.As(err, &diagnostic) || diagnostic.ReasonCode != code || !errors.Is(err, cause) || !receipt.VerifiedAt.IsZero() || strings.Contains(err.Error(), "private-identity-canary") {
					t.Fatalf("err=%v receipt=%+v", err, receipt)
				}
				wantReads, wantOpens, wantClosed := 0, 0, 0
				if boundary == 2 {
					wantReads, wantOpens, wantClosed = 2, 1, 1
				}
				if reads.Load() != int32(wantReads) || client.opens != wantOpens || closed != wantClosed || client.calls != boundary {
					t.Fatalf("retried or skipped reads/cleanup: reads=%d opens=%d closed=%d calls=%d", reads.Load(), client.opens, closed, client.calls)
				}
				if apierrors.IsNotFound(err) != (scenario == "not found") || apierrors.IsConflict(err) != (scenario == "conflict") {
					t.Fatal("private classification lost")
				}
			})
		}
	}
}
