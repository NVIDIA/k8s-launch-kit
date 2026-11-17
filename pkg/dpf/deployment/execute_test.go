// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type executionTestFile struct {
	bytes.Buffer
	syncs, failAt int
}

func (f *executionTestFile) Sync() error {
	f.syncs++
	if f.syncs == f.failAt {
		return errors.New("receipt disk unavailable")
	}
	return nil
}
func (f *executionTestFile) Close() error { return nil }

type executionTestAPI struct {
	file            *executionTestFile
	current         *unstructured.Unstructured
	gets, creates   int
	loseCreateReply bool
	replaceOnRead   bool
	t               *testing.T
}

func (c *executionTestAPI) Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	c.gets++
	if c.current == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "operator.dpu.nvidia.com", Resource: "dpfoperatorconfigs"}, "dpfoperatorconfig")
	}
	out := c.current.DeepCopy()
	if c.replaceOnRead {
		out.SetUID("replacement-uid")
	}
	out.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1), "reason": "Ready"}}}
	return out, nil
}
func (c *executionTestAPI) Create(_ context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if c.file.syncs != 1 || !strings.Contains(c.file.String(), `"state":"attempted"`) || dryRun {
		c.t.Fatal("effect occurred without a durable attempt or used unexpected dry-run")
	}
	c.creates++
	c.current = desired.DeepCopy()
	c.current.SetUID("created-uid")
	c.current.SetResourceVersion("1")
	c.current.SetGeneration(1)
	if c.loseCreateReply {
		return nil, errors.New("response lost after server committed secret-value")
	}
	return c.current.DeepCopy(), nil
}

func TestNativeObjectExecutionRetainsAttemptsAndStopsAfterPartialFailure(t *testing.T) {
	request := operatorTestRequest(t)
	rendered, err := bootstrap.RenderOperator(request.Archive, request.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.dpu.nvidia.com/v1alpha1", "kind": "DPFOperatorConfig",
		"metadata": map[string]any{"name": "dpfoperatorconfig", "namespace": "dpf"},
		"spec":     map[string]any{"deploymentMode": "zero-trust", "provisioningController": map[string]any{"installInterface": map[string]any{"installViaRedfish": map[string]any{"discoveredDPUDeviceBMCFactoryResetPolicy": "Never"}}}},
	}}
	reconciler, err := NewReconciler(rendered.Manifest, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.markedDesired(desired); err != nil {
		t.Fatalf("test desired config invalid: %v", err)
	}
	for _, tc := range []struct {
		name            string
		failSync        int
		loseCreateReply bool
		replaceOnRead   bool
		wrongDigest     bool
		wantCreates     int
		wantGets        int
		wantSuccess     bool
	}{
		{name: "ordered create and readiness", wantCreates: 1, wantGets: 2, wantSuccess: true},
		{name: "attempt persistence failure", failSync: 1, wantCreates: 0, wantGets: 0},
		{name: "receipt persistence failure", failSync: 2, wantCreates: 1, wantGets: 1},
		{name: "lost create reply", loseCreateReply: true, wantCreates: 1, wantGets: 1},
		{name: "resource replaced while waiting", replaceOnRead: true, wantCreates: 1, wantGets: 2},
		{name: "report identifies another bundle", wrongDigest: true, wantCreates: 0, wantGets: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f := &executionTestFile{failAt: tc.failSync}
			journal := &executionJournal{file: f, digest: "sha256:" + strings.Repeat("a", 64)}
			if tc.wrongDigest {
				journal.digest = "sha256:" + strings.Repeat("b", 64)
			}
			client := &executionTestAPI{file: f, t: t, loseCreateReply: tc.loseCreateReply, replaceOnRead: tc.replaceOnRead}
			obj, err := executeNativeObject(ctx, client, reconciler, desired, "dpf-operator-config", journal, true, time.Millisecond)
			if (err == nil) != tc.wantSuccess || client.gets != tc.wantGets || client.creates != tc.wantCreates {
				t.Fatalf("result err=%v gets=%d creates=%d, want success=%v gets=%d creates=%d", err, client.gets, client.creates, tc.wantSuccess, tc.wantGets, tc.wantCreates)
			}
			if tc.wantSuccess && (obj == nil || obj.GetUID() != "created-uid" || !EvaluateReadiness(obj).Ready) {
				t.Fatal("success did not carry established ready resource")
			}
			if strings.Contains(f.String(), "secret-value") || err != nil && strings.Contains(err.Error(), "secret-value") {
				t.Fatal("raw API failure escaped safe reporting")
			}
			if tc.loseCreateReply {
				decoder := json.NewDecoder(bytes.NewReader(f.Bytes()))
				var first, last executionEvent
				if decoder.Decode(&first) != nil || decoder.Decode(&last) != nil || first.State != "attempted" || last.State != "uncertain" || last.ReasonCode != "RESOURCE_CREATE_UNCERTAIN" {
					t.Fatal("partial write did not retain attempt and failure")
				}
			}
			if tc.replaceOnRead {
				lines := bytes.Split(bytes.TrimSpace(f.Bytes()), []byte("\n"))
				var last executionEvent
				if json.Unmarshal(lines[len(lines)-1], &last) != nil || last.State != "failed" || last.ReasonCode != "RESOURCE_IDENTITY_CHANGED" {
					t.Fatalf("terminal failure was not durably classified: %+v", last)
				}
			}
			if tc.wrongDigest && f.Len() != 0 {
				t.Fatal("mismatched bundle attempted an operation")
			}
		})
	}
}
