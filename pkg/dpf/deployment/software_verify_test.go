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
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type softwareVerificationFake struct {
	current                                 *unstructured.Unstructured
	tunnel                                  *registryTunnel
	managerImage                            string
	reads, snapshots, opens, identityChecks int
	mutateOnRead                            func(int, *unstructured.Unstructured)
	changeCA                                bool
	failIdentityAt                          int
	startupError                            error
}

func (f *softwareVerificationFake) Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.reads++
	current := f.current.DeepCopy()
	if f.mutateOnRead != nil {
		f.mutateOnRead(f.reads, current)
	}
	return current, nil
}

func (f *softwareVerificationFake) VerifyIdentity(context.Context) error {
	f.identityChecks++
	if f.identityChecks == f.failIdentityAt {
		return errors.New("changed management cluster")
	}
	return nil
}

func (f *softwareVerificationFake) observeRegistrySnapshot(context.Context, string) (registrySnapshot, error) {
	f.snapshots++
	// This fake isolates orchestration from the separately tested native reader.
	ca := []byte("stable public trust")
	if f.changeCA && f.snapshots > 1 {
		ca = []byte("changed public trust")
	}
	return registrySnapshot{caPEM: ca, manager: registryManagerBinding{container: registryContainerBinding{image: f.managerImage}}}, nil
}

func (f *softwareVerificationFake) openSoftwareRegistry(context.Context, string, []byte) (*registryTunnel, error) {
	f.opens++
	if f.startupError != nil {
		return nil, f.startupError
	}
	return f.tunnel, nil
}

type softwareVerifierFunc func(*unstructured.Unstructured, *unstructured.Unstructured) error

func (f softwareVerifierFunc) Verify(desired, current *unstructured.Unstructured) error {
	return f(desired, current)
}

func TestStagedSoftwareVerificationBracketsStreamsAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		changeAt                                             int
		changeCA, cleanupFail, digestMismatch, invalidIntent bool
		startupCleanupFailure, wrongManagerImage             bool
		failIdentityAt                                       int
		wantPass                                             bool
	}{
		{name: "matching selected bytes", wantPass: true},
		{name: "manager image differs from selected release", wrongManagerImage: true},
		{name: "software replaced before reading", changeAt: 1},
		{name: "software replaced after reading", changeAt: 2},
		{name: "public trust changed", changeCA: true},
		{name: "cleanup failed", cleanupFail: true},
		{name: "stored bytes differ", digestMismatch: true},
		{name: "initial ownership invalid", invalidIntent: true},
		{name: "management changed after reading", failIdentityAt: 2},
		{name: "startup cleanup failure retained", startupCleanupFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired, current, locks, selected := softwareCacheFixture()
			osData, pldmData := "selected OS image", "selected firmware"
			locks.OSISO.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(osData)))
			pldm := locks.PLDM["mt_testpsid"]
			pldm.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(pldmData)))
			locks.PLDM["mt_testpsid"] = pldm
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				data := osData
				if strings.HasPrefix(r.URL.Path, "/bfb/components/") {
					data = pldmData
				}
				if tc.digestMismatch {
					data = "other bytes"
				}
				_, _ = io.WriteString(w, data)
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			port, _ := strconv.Atoi(endpoint.Port())
			closed := 0
			managerImage := "nvcr.io/nvidia/doca/dpf-system:v26.8.0"
			if tc.wrongManagerImage {
				managerImage = "nvcr.io/nvidia/doca/dpf-system:v26.4.0"
			}
			fake := &softwareVerificationFake{current: current, managerImage: managerImage, changeCA: tc.changeCA, failIdentityAt: tc.failIdentityAt}
			if tc.startupCleanupFailure {
				fake.startupError = errors.Join(context.Canceled, errors.New("registry port-forward cleanup timed out"))
			}
			fake.tunnel = &registryTunnel{port: port, client: server.Client(), closeFn: func() error {
				closed++
				if tc.cleanupFail {
					return errors.New("registry tunnel cleanup did not complete")
				}
				return nil
			}}
			fake.mutateOnRead = func(read int, obj *unstructured.Unstructured) {
				if read == tc.changeAt {
					obj.SetUID("replacement-software")
				}
			}
			verify := softwareVerifierFunc(func(want, got *unstructured.Unstructured) error {
				if tc.invalidIntent || !reflect.DeepEqual(want.Object["spec"], got.Object["spec"]) {
					return errors.New("invalid ownership or intent")
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			receipt, err := verifyStagedSoftware(ctx, fake, verify, desired, current, locks, selected, "operator", "nvcr.io/nvidia/doca/dpf-system:v26.8.0", 1024)
			if tc.wantPass {
				if err != nil || receipt.SoftwareUID != current.GetUID() || len(receipt.Components) != 2 || receipt.VerifiedAt.IsZero() || requests.Load() != 2 || fake.reads != 2 || fake.snapshots != 2 || fake.identityChecks != 2 {
					t.Fatalf("incomplete receipt or verification: %#v, %v", receipt, err)
				}
				if receipt.Components[0].Bytes != int64(len(osData)) || receipt.Components[1].Bytes != int64(len(pldmData)) {
					t.Fatal("incorrect byte counts")
				}
			} else if err == nil || receipt.SoftwareUID != "" || len(receipt.Components) != 0 {
				t.Fatalf("failed check returned accepted receipt: %#v, %v", receipt, err)
			}
			if tc.startupCleanupFailure {
				if requests.Load() != 0 || closed != 0 || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "cleanup timed out") {
					t.Fatalf("startup cleanup uncertainty was lost: %v", err)
				}
			} else if tc.invalidIntent || tc.changeAt == 1 || tc.wrongManagerImage {
				if fake.opens != 0 || requests.Load() != 0 || closed != 0 {
					t.Fatal("invalid initial binding opened registry transport")
				}
			} else if closed != 1 {
				t.Fatalf("cleanup count = %d, want one", closed)
			}
		})
	}
}
