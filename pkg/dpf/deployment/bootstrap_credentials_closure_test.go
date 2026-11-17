// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBootstrapCredentialClosureAllowsUnrelatedChurn(t *testing.T) {
	for _, kind := range []string{"other-token signature", "token metadata and description", "join metadata and unrelated data"} {
		t.Run(kind, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			originalJoin := m.join.DeepCopy()
			selectedSignature := s.info.Data["jws-kubeconfig-"+bootstrapTestID]
			if kind == "join metadata and unrelated data" {
				m.beforeRead = func(n int) {
					if n == 3 {
						m.join.SetResourceVersion("2")
						m.join.SetAnnotations(map[string]string{"unrelated": "updated"})
						m.join.Object["data"].(map[string]any)["unrelated"] = "dXBkYXRlZA=="
					}
				}
			} else {
				s.beforeRead = func(path string, n int) {
					if n != 2 {
						return
					}
					switch {
					case kind == "other-token signature" && strings.Contains(path, "/configmaps/"):
						s.info.ResourceVersion = "2"
						s.info.Data["jws-kubeconfig-other1"] = "other-token-signature"
						s.info.Annotations = map[string]string{"unrelated": "updated"}
					case kind == "token metadata and description" && strings.Contains(path, "/secrets/"):
						s.token.ResourceVersion = "2"
						s.token.Annotations = map[string]string{"unrelated": "updated"}
						s.token.Data["description"] = []byte("updated description")
					}
				}
			}
			if err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, nil); err != nil {
				t.Fatalf("unrelated %s rejected: %v", kind, err)
			}
			if m.writes != 0 || s.writes != 0 || s.info.Data["jws-kubeconfig-"+bootstrapTestID] != selectedSignature || originalJoin.GetUID() != m.join.GetUID() {
				t.Fatal("authority changed or guard mutated resources")
			}
		})
	}
}

func TestBootstrapCredentialClosureRejectsSelectedChanges(t *testing.T) {
	for _, kind := range []string{"join UID", "join owner", "join value", "join deleting", "token UID", "token value", "token ID", "token usage", "token groups", "token labels", "token expiry", "token deleting", "info UID", "info kubeconfig", "info selected signature", "info deleting"} {
		t.Run(kind, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			if strings.HasPrefix(kind, "join") {
				m.beforeRead = func(n int) {
					if n != 3 {
						return
					}
					switch kind {
					case "join UID":
						m.join.SetUID("replacement")
					case "join owner":
						owners := m.join.GetOwnerReferences()
						owners[0].UID = "foreign"
						m.join.SetOwnerReferences(owners)
					case "join value":
						m.join.Object["data"].(map[string]any)["join"] = "Y2hhbmdlZA=="
					case "join deleting":
						now := metav1.Now()
						m.join.SetDeletionTimestamp(&now)
					}
				}
			} else {
				s.beforeRead = func(path string, n int) {
					if n != 2 {
						return
					}
					if strings.HasPrefix(kind, "token") && strings.Contains(path, "/secrets/") {
						switch kind {
						case "token UID":
							s.token.UID = "replacement"
						case "token value":
							s.token.Data["token-secret"] = []byte("changed")
						case "token ID":
							s.token.Data["token-id"] = []byte("changed")
						case "token usage":
							s.token.Data["usage-bootstrap-authentication"] = []byte("false")
						case "token groups":
							s.token.Data["auth-extra-groups"] = []byte("foreign")
						case "token labels":
							s.token.Labels["provisioning.dpu.nvidia.com/dpu-name"] = "foreign"
						case "token expiry":
							s.token.Data["expiration"] = []byte(time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339))
						case "token deleting":
							now := metav1.Now()
							s.token.DeletionTimestamp = &now
						}
					}
					if strings.HasPrefix(kind, "info") && strings.Contains(path, "/configmaps/") {
						switch kind {
						case "info UID":
							s.info.UID = "replacement"
						case "info kubeconfig":
							s.info.Data["kubeconfig"] += "\n"
						case "info selected signature":
							s.info.Data["jws-kubeconfig-"+bootstrapTestID] = "changed"
						case "info deleting":
							now := metav1.Now()
							s.info.DeletionTimestamp = &now
						}
					}
				}
			}
			if err := verifyAbsentRegistrationCredentials(context.Background(), m, c, dpu, nil); err == nil {
				t.Fatalf("selected %s change accepted", kind)
			}
			if m.writes != 0 || s.writes != 0 {
				t.Fatal("guard wrote resources")
			}
		})
	}
}
