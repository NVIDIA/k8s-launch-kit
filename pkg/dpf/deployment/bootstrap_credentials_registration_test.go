// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Models the producer order: registration becomes Ready, DPF consumes its
// credentials; that same registration may subsequently be NotReady. Presence
// excuses only this predicate, not independent downstream readiness.
func bootstrapArrivedNode(name string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "registration-arrived", ResourceVersion: "1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !ready {
		n.ResourceVersion = "2"
		n.Status.Conditions[0].Status = corev1.ConditionFalse
	}
	return n
}

func TestBootstrapRegistrationProgressDuringCredentialCleanup(t *testing.T) {
	for _, stage := range []string{"join first read", "token read", "JWS read", "join reread"} {
		for _, ready := range []bool{true, false} {
			name := stage + "/Ready"
			if !ready {
				name = stage + "/registered then NotReady"
			}
			t.Run(name, func(t *testing.T) {
				m, c, s, dpu := newBootstrapCredentialFixture(t)
				transitioned := false
				transition := func() {
					s.node = bootstrapArrivedNode(dpu.GetName(), ready)
					s.token = nil
					delete(s.info.Data, "jws-kubeconfig-"+bootstrapTestID)
					transitioned = true
				}
				if stage == "join first read" || stage == "join reread" {
					m.beforeRead = func(n int) {
						want := 2
						if stage == "join reread" {
							want = 3
						}
						if n == want {
							s.mu.Lock()
							transition()
							s.mu.Unlock()
							m.join = nil
						}
					}
				} else {
					expected, sources := c.expected, c.source.(*tenantSourceStub).sources
					handler := func(w http.ResponseWriter, r *http.Request) bool {
						s.mu.Lock()
						if !transitioned && (stage == "token read" && strings.Contains(r.URL.Path, "/secrets/") || stage == "JWS read" && strings.Contains(r.URL.Path, "/configmaps/")) {
							transition()
						}
						s.mu.Unlock()
						return s.serve(w, r)
					}
					c, _ = tenantIdentityClientFixtureWithHandler(t, expected, &tenantSourceStub{sources: sources}, handler)
				}
				file := &managementMemoryFile{}
				journal := &executionJournal{file: file, digest: "test"}
				err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, journal)
				if strings.Contains(file.String(), `"state":"failed"`) {
					t.Fatalf("normal progress recorded a failure: %s", file.String())
				}
				if !transitioned {
					t.Fatal("did not execute registration/cleanup producer transition")
				}
				if err != nil {
					t.Fatalf("normal arrived registration rejected during %s: %v", stage, err)
				}
				if m.writes != 0 || s.writes != 0 {
					t.Fatal("guard mutated resources")
				}
			})
		}
	}
}

type bootstrapJoinWaitRegistrationClient struct {
	bootstrapJoinWaitClient
	state        *bootstrapTenantFixture
	joinKey      string
	transitioned bool
}

func (c *bootstrapJoinWaitRegistrationClient) Get(ctx context.Context, want *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if tenantStageKey(want) == c.joinKey && !c.transitioned {
		c.state.mu.Lock()
		c.state.node = bootstrapArrivedNode(c.state.nodeName, true)
		c.state.token = nil
		delete(c.state.info.Data, "jws-kubeconfig-"+bootstrapTestID)
		c.state.mu.Unlock()
		delete(c.base.resources, c.joinKey)
		c.transitioned = true
	}
	return c.recoveryActivationClient.Get(ctx, want)
}

func TestBootstrapActivationWaitAllowsRegistrationCleanup(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	item := f.client.activationSelected[0]
	dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU"}}
	dpu.SetName(item.dpuName)
	dpu.SetNamespace(f.prepared.execution.operatorNamespace)
	dpu.SetUID(activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial))
	m, state := bootstrapCredentialMaterials(t, dpu, f.client.sources.sources.secret.Data["super-admin.conf"])
	key := tenantStageKey(m.join)
	f.client.base.resources[key] = m.join
	f.client.bootstrapTenantHandler = state.serve
	client := &bootstrapJoinWaitRegistrationClient{bootstrapJoinWaitClient: bootstrapJoinWaitClient{recoveryActivationClient{f.client}}, state: state, joinKey: key}
	testTenant(f.controlPlane.tenant.Client).source = client
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	reboot := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	if !client.transitioned {
		t.Fatalf("registration/cleanup transition not reached: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "BOOTSTRAP_") {
		t.Fatalf("actual join-wait reader misdiagnosed normal consumed credentials after registration: %v", err)
	}
	if reboot.submissions != 0 {
		t.Fatalf("registration proof enabled reset: %d", reboot.submissions)
	}
}

func TestBootstrapRegistrationProgressNeverWaivesUnverifiedIdentity(t *testing.T) {
	for _, part := range []string{"tenant drift", "DPU drift", "registration forbidden", "registration replaced", "cancel before recheck", "cancel during recheck"} {
		t.Run(part, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.beforeRead = func(n int) {
				if n == 2 {
					s.mu.Lock()
					s.node = bootstrapArrivedNode(dpu.GetName(), true)
					s.mu.Unlock()
					m.join = nil
					switch part {
					case "tenant drift":
						c.source.(*tenantSourceStub).sources.observed.DPUCluster.SetUID("foreign")
					case "DPU drift":
						m.dpu.SetUID("foreign")
					case "registration forbidden":
						s.forbidden = "/api/v1/nodes/" + dpu.GetName()
					case "cancel before recheck":
						cancel()
					}
				}
			}
			s.beforeRead = func(path string, n int) {
				if path == "/api/v1/nodes/"+dpu.GetName() {
					if part == "registration replaced" && n == 3 {
						s.node.UID = "replacement"
					}
					if part == "cancel during recheck" && n == 2 {
						cancel()
					}
				}
			}
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			err := verifyAbsentRegistrationCredentials(ctx, m, c, dpu, journal)
			if err == nil {
				t.Fatal("registration waived invalid identity or canceled inspection")
			}
			if strings.HasPrefix(part, "cancel") && !strings.Contains(err.Error()+file.String(), "timeout-or-canceled") {
				t.Fatal("cancellation lost its safe read cause")
			}
			if !strings.Contains(file.String(), `"state":"failed"`) {
				t.Fatal("unverified identity failure not retained")
			}
			if m.writes != 0 || s.writes != 0 || strings.Contains(file.String(), bootstrapTestSecret) {
				t.Fatal("effect or private credential leaked")
			}
		})
	}
}
