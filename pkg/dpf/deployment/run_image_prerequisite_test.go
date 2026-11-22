// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/types"
)

// Existing prerequisite-boundary tests stop before management and do not need
// network metadata. Real registry transport behavior is tested below.
func retainedTestImageResolver(_ context.Context, reference string, _ time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
	_, digest, _ := strings.Cut(reference, "@")
	return hbnPodImageIdentity{Reference: reference, ManifestDigest: digest, ConfigDigest: "sha256:" + strings.Repeat("b", 64)}, nil
}

func imagePrerequisiteRunRequest(t *testing.T, plan *generation.Plan, prerequisites PrerequisiteVerifier) RunRequest {
	t.Helper()
	root := t.TempDir()
	return RunRequest{PublishAcceptance: testAcceptancePublisher(t), Plan: plan, Access: config.Request{Kubeconfig: filepath.Join(root, "must-not-be-opened")},
		ReportPath: filepath.Join(root, "run.jsonl"), ProbeJournalPath: filepath.Join(root, "probes.json"),
		VerifyPrerequisites: prerequisites,
		VerifyChartSource: func(context.Context, *generation.Plan) (PreflightCheck, error) {
			t.Fatal("chart access after image prerequisite stop")
			return PreflightCheck{}, nil
		},
		Rebooter: runRebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
			t.Fatal("reset submission after image prerequisite stop")
			return ActivationObservation{}, nil
		}),
		RequestTimeout: time.Second,
	}
}

func TestRunImageMetadataFailureStopsFreshAndResumeBeforeEffects(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan := requireRunPlan(t, retained)
	for _, resume := range []bool{false, true} {
		for _, scenario := range []string{"private-basic", "foreign-bearer", "unavailable", "wrong-digest"} {
			t.Run(fmt.Sprintf("resume-%t/%s", resume, scenario), func(t *testing.T) {
				request := imagePrerequisiteRunRequest(t, plan, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
					t.Fatal("discovery/probes/selector work ran after failed metadata")
					return PrerequisiteVerification{}, nil
				})
				if resume {
					request.Resume = &OperationRef{Namespace: "dpf-operator-system", Name: "dpf-op-original", UID: types.UID("123e4567-e89b-12d3-a456-426614174006")}
				}
				requests := 0
				transport := hbnImageTransportFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					if requests != 1 || r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "nvcr.io" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Fatalf("unexpected registry request: %s %s", r.Method, r.URL)
					}
					if scenario == "unavailable" {
						return nil, errors.New("remote-secret-canary")
					}
					header := http.Header{}
					status := http.StatusUnauthorized
					switch scenario {
					case "private-basic":
						header.Set("WWW-Authenticate", `Basic realm="private"`)
					case "foreign-bearer":
						header.Set("WWW-Authenticate", `Bearer realm="https://foreign.example/token",service="registry"`)
					case "wrong-digest":
						status = http.StatusOK
					}
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				result, err := runWithTransports(ctx, request, runTransports{resolveImage: func(ctx context.Context, ref string, timeout time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
					return resolveHBNRuntimeImage(ctx, ref, timeout, transport)
				}})
				if err == nil || !strings.Contains(err.Error(), "HBN_IMAGE_METADATA_UNAVAILABLE") || !strings.Contains(err.Error(), "anonymous HTTPS") || requests != 1 {
					t.Fatalf("failure not rejected early: result=%+v err=%v reads=%d", result, err, requests)
				}
				if result.State != RunStateFailed || result.TrafficValidated || result.LastCompletedStage != "" || result.Operation != nil || result.Activation != nil || len(result.HostNetworking) != 0 || result.HBNRuntime != nil {
					t.Fatalf("image failure retained later stage/effect: %+v", result)
				}
				if len(result.Checks) < 1 || result.Checks[0].ReasonCode != "HBN_IMAGE_METADATA_UNAVAILABLE" || result.Checks[0].State != "unknown" {
					t.Fatalf("missing image prerequisite result: %+v", result.Checks)
				}
				report, err := os.ReadFile(request.ReportPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(report), "HBN_IMAGE_METADATA_UNAVAILABLE") || strings.Contains(string(report), "FRESH_PREREQUISITES_STARTED") || strings.Contains(string(report), "remote-secret-canary") {
					t.Fatalf("unsafe/missing early report: %s", report)
				}
				probes, err := os.ReadFile(request.ProbeJournalPath)
				if err != nil || !strings.Contains(string(probes), `"receipts":[]`) {
					t.Fatalf("probe resources changed: %s %v", probes, err)
				}
			})
		}
	}
}

func TestRunImageMetadataAcceptsRetainedAnonymousRepositoryBeforeDiscovery(t *testing.T) {
	defaults := preflightGenerationInputs(t).Workload.Generation
	for _, repository := range []string{defaults.ImageRepository, "registry.example:5000/team/hbn"} {
		t.Run(repository, func(t *testing.T) {
			api := vacantOperationAPIFixture(t)
			input := operationRunInputs(t, api)
			configData := imageMetadataJSON(map[string]any{"os": "linux", "architecture": "arm64"})
			configDigest := imageMetadataDigest(configData)
			manifest := imageMetadataJSON(map[string]any{"schemaVersion": 2, "mediaType": hbnManifestMediaType, "config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(configData)}, "layers": []any{}})
			input.Workload.Generation.ImageRepository = repository
			input.Workload.Generation.ImageDigest = imageMetadataDigest(manifest)
			retained, err := generateFixtureBundle(input)
			if err != nil {
				t.Fatal(err)
			}
			plan := requireRunPlan(t, retained)
			expected := hbnImageReference(input.Workload.Generation)
			host, path, _ := strings.Cut(repository, "/")
			metadataReads, tokens, prerequisites := 0, 0, 0
			transport := hbnImageTransportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != host || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
					t.Fatalf("metadata authority changed: %s", r.URL)
				}
				header := http.Header{}
				status := http.StatusOK
				data := manifest
				if r.URL.Path == "/token" {
					tokens++
					if r.URL.Query().Get("scope") != "repository:"+path+":pull" || r.Header.Get("Authorization") != "" {
						t.Fatal("token scope/access changed")
					}
					data = []byte(`{"token":"anonymous-test-token"}`)
				} else if r.Header.Get("Authorization") == "" {
					status = http.StatusUnauthorized
					header.Set("WWW-Authenticate", `Bearer realm="https://`+host+`/token",service="registry"`)
				} else {
					metadataReads++
					if strings.Contains(r.URL.Path, "/blobs/") {
						data = configData
					} else if !strings.HasSuffix(r.URL.Path, input.Workload.Generation.ImageDigest) {
						t.Fatalf("not reading exact retained digest: %s", r.URL)
					}
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(string(data))), ContentLength: int64(len(data)), Request: r}, nil
			})
			request := imagePrerequisiteRunRequest(t, plan, func(_ context.Context, _ config.Request, plan *generation.Plan, _ PrerequisitePhase) (PrerequisiteVerification, error) {
				prerequisites++
				if metadataReads != 2 || tokens != 1 || hbnImageReference(plan.Intent().Generation) != expected {
					t.Fatal("discovery preceded exact retained metadata identity")
				}
				return PrerequisiteVerification{}, errors.New("stop after proven early metadata")
			})
			request.Access = api.request
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err = runWithTransports(ctx, request, runTransports{resolveImage: func(ctx context.Context, ref string, timeout time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
				if ref != expected {
					t.Fatalf("retained override rewritten: %s", ref)
				}
				return resolveHBNRuntimeImage(ctx, ref, timeout, transport)
			}})
			if err == nil || !strings.Contains(err.Error(), "prerequisite observation failed") || prerequisites != 1 {
				t.Fatalf("positive metadata did not reach prerequisite boundary: %v", err)
			}
			report, err := os.ReadFile(request.ReportPath)
			if err != nil || !strings.Contains(string(report), "HBN_IMAGE_METADATA_VERIFIED") {
				t.Fatalf("early success not recorded: %s %v", report, err)
			}
		})
	}
}

func TestRunEarlyImageMetadataSuccessPrecedesManagementAccess(t *testing.T) {
	fixture := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	calls := 0
	resolver := func(_ context.Context, ref string, _ time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
		calls++
		if ref != fixture.image.Reference {
			t.Fatal("early/runtime references differ")
		}
		if calls == 1 {
			return fixture.image, nil
		}
		return hbnPodImageIdentity{}, errors.New("metadata became unavailable")
	}
	request := imagePrerequisiteRunRequest(t, fixture.prepared.execution.plan, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
		return PrerequisiteVerification{}, errors.New("stop after early metadata")
	})
	_, err := runWithTransports(fixture.ctx, request, runTransports{resolveImage: resolver})
	// This request deliberately has no management access. The early image read
	// must complete before the new core operation inspection can reject it.
	if err == nil || !strings.Contains(err.Error(), "OPERATION_MANAGEMENT_UNMEASURED") || calls != 1 {
		t.Fatalf("early read not established: calls=%d err=%v", calls, err)
	}
	// The native Run acceptance fixture separately verifies a fresh second image read.
}
