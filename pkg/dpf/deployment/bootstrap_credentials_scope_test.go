// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type bootstrapSubmissionScopeClient struct {
	recoveryActivationClient
	armed      atomic.Bool
	scopeReads int
	mode       string
	injected   bool
	cancel     context.CancelFunc
}

func (c *bootstrapSubmissionScopeClient) List(ctx context.Context, want *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	objects, err := c.recoveryActivationClient.List(ctx, want, selector)
	if err == nil && c.armed.Load() && want.GetKind() == "DPUSet" {
		c.scopeReads++
		// First is the post-credential resource fence; second is the explicitly
		// collected final reset scope, after that fence and before baseline/CAS.
		if c.scopeReads == 2 {
			c.injected = true
			switch c.mode {
			case "missing":
				return nil, nil
			case "partial":
				return objects[:len(objects)-1], nil
			case "canceled":
				c.cancel()
				return nil, context.Canceled
			}
		}
	}
	return objects, err
}

func TestBootstrapBeforeSubmitRequiresCompleteCurrentScope(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, mode := range []string{"missing", "partial", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			f.ctx = ctx
			operation, _ := checkpointFixture(t, f)
			f.prepared.operation = operation
			// This fixture has expected-name registrations, excusing only the
			// credential predicate. Final scope completeness remains mandatory.
			client := &bootstrapSubmissionScopeClient{recoveryActivationClient: recoveryActivationClient{f.client}, mode: mode, cancel: cancel}
			f.controlPlane.tenant.Client.source = client
			reboot := &recoveryRebooter{fixture: &f, beforeBaseline: func() { client.armed.Store(true) }}
			_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
			if err == nil || !client.injected || reboot.submissions != 0 || len(f.client.guards) != 1 {
				t.Fatalf("scope=%s injected=%v claims=%d submissions=%d err=%v", mode, client.injected, len(f.client.guards), reboot.submissions, err)
			}
			for _, reset := range f.prepared.operation.state.Resets {
				if reset.Baseline != "" || reset.Acknowledged {
					t.Fatal("incomplete/canceled scope crossed baseline or ACK")
				}
			}
		})
	}
}
