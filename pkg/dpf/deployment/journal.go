// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// executionEvent contains only lifecycle identifiers and bounded, safe check
// results. Never put resource bodies, credentials, or raw API errors here.
type executionEvent struct {
	Sequence     uint64    `json:"sequence"`
	Time         time.Time `json:"time"`
	BundleDigest string    `json:"bundleDigest"`
	Stage        string    `json:"stage"`
	Operation    string    `json:"operation"`
	Subject      string    `json:"subject,omitempty"`
	State        string    `json:"state"`
	ReasonCode   string    `json:"reasonCode"`
	Message      string    `json:"message"`
}

type journalFile interface {
	io.Writer
	Sync() error
	Close() error
}

// executionJournal is append-only and never authorizes resuming an effect.
// A durable attempt without a durable result means the effect is uncertain;
// a subsequent run must inspect the actual cluster again.
type executionJournal struct {
	mu       sync.Mutex
	file     journalFile
	digest   string
	sequence uint64
	failed   error
	closed   bool
}

func newExecutionJournal(filename, digest string) (*executionJournal, error) {
	if filename == "" || digest == "" {
		return nil, fmt.Errorf("execution report path and bundle digest are required")
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create new execution report: %w", err)
	}
	j := &executionJournal{file: f, digest: digest}
	if err := j.record(executionEvent{Operation: "run", State: "started", ReasonCode: "EXECUTION_STARTED", Message: "Execution started; no deployment readiness has been established."}); err != nil {
		_ = j.close()
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(filename))
	if err == nil {
		err = errors.Join(dir.Sync(), dir.Close())
	}
	if err != nil {
		_ = j.close()
		return nil, fmt.Errorf("persist execution report directory: %w", err)
	}
	return j, nil
}

// record must succeed before each effect. After any write or sync error this
// journal is unusable, even if a later write might succeed: the preceding
// receipt may be incomplete and must not be mistaken for durable evidence.
func (j *executionJournal) record(event executionEvent) error {
	if j == nil {
		return fmt.Errorf("execution report is unavailable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failed != nil {
		return j.failed
	}
	if j.closed || j.file == nil {
		return fmt.Errorf("execution report is closed")
	}
	event.Sequence = j.sequence + 1
	event.Time = time.Now().UTC()
	event.BundleDigest = j.digest
	data, err := json.Marshal(event)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = j.file.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.failed = fmt.Errorf("persist execution report; stop before further effects: %w", err)
		return j.failed
	}
	j.sequence = event.Sequence
	return nil
}

func (j *executionJournal) close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return j.failed
	}
	j.closed = true
	if j.file == nil {
		return j.failed
	}
	j.failed = errors.Join(j.failed, j.file.Close())
	return j.failed
}
