// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionJournalDurableOrderedReceipts(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "execution.jsonl")
	j, err := newExecutionJournal(filename, "sha256:bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.close() }()
	for _, state := range []string{"attempted", "confirmed"} {
		if err := j.record(executionEvent{Stage: "management-namespaces", Operation: "create", Subject: "Namespace/dpf", State: state, ReasonCode: "RESOURCE_CREATE", Message: "Create receipt."}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for i := uint64(1); i <= 3; i++ {
		var event executionEvent
		if err := decoder.Decode(&event); err != nil || event.Sequence != i || event.BundleDigest != "sha256:bundle" || event.Time.IsZero() {
			t.Fatalf("incorrect durable event %d: %+v, %v", i, event, err)
		}
	}
	if err := decoder.Decode(new(executionEvent)); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected trailing receipt: %v", err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report permissions: %v %v", info, err)
	}
	if _, err := newExecutionJournal(filename, "another-bundle"); err == nil {
		t.Fatal("reused an existing run report")
	}
	retained, _ := os.ReadFile(filename)
	if !bytes.Equal(data, retained) {
		t.Fatal("existing run report was changed")
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	if err := j.record(executionEvent{}); err == nil {
		t.Fatal("accepted receipt after close")
	}
}

type failingJournalFile struct {
	writes, syncs int
	shortWrite    bool
	writeErr      error
	syncErr       error
}

func (f *failingJournalFile) Write(data []byte) (int, error) {
	f.writes++
	if f.shortWrite {
		return len(data) - 1, nil
	}
	return len(data), f.writeErr
}
func (f *failingJournalFile) Sync() error  { f.syncs++; return f.syncErr }
func (f *failingJournalFile) Close() error { return nil }

func TestExecutionJournalPersistenceFailureStopsFurtherReceipts(t *testing.T) {
	failure := errors.New("storage unavailable")
	for _, tc := range []struct {
		name string
		file failingJournalFile
	}{
		{name: "write", file: failingJournalFile{writeErr: failure}},
		{name: "short write", file: failingJournalFile{shortWrite: true}},
		{name: "sync", file: failingJournalFile{syncErr: failure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &executionJournal{file: &tc.file, digest: "sha256:bundle"}
			if err := j.record(executionEvent{State: "attempted"}); err == nil {
				t.Fatal("accepted non-durable attempt")
			}
			if err := j.record(executionEvent{State: "confirmed"}); err == nil || tc.file.writes != 1 || j.sequence != 0 {
				t.Fatal("continued after persistence failure")
			}
		})
	}
}
