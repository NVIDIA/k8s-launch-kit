// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package artifacts publishes secret-free discovery files on local filesystems.
package artifacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/client-go/tools/clientcmd"
)

type Snapshot struct {
	Exists bool
	Data   []byte
}
type Store struct {
	Output         string
	RunDir         string
	Input          config.Input
	Initial        Snapshot
	PriorInventory *inventory.Inventory
}

func New(in config.Input, destination string, explicit bool) (*Store, error) {
	if destination == "" {
		destination = filepath.Join(filepath.Dir(in.Path), "dpf-discovered.yaml")
	}
	output, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	input, err := filepath.Abs(in.Path)
	if err != nil {
		return nil, err
	}
	if input == output && !explicit {
		return nil, fmt.Errorf("in-place refresh requires explicit --save-cluster-config")
	}
	if err := safePath(output); err != nil {
		return nil, err
	}
	initial, err := snapshot(output)
	if err != nil {
		return nil, err
	}
	if initial.Exists && output != input {
		old, e := config.Decode(initial.Data, output)
		if e != nil || old.Site.Inventory == nil || !sameRequested(old.Site, in.Site) {
			return nil, fmt.Errorf("output exists and is not a DPF inventory for the same requested scope")
		}
	}
	// Protect source and referenced private files, including aliases via symlinks/hardlinks.
	sourceInfo, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect source")
	}
	outputInfo, _ := os.Stat(output)
	if outputInfo != nil && os.SameFile(sourceInfo, outputInfo) && input != output {
		return nil, fmt.Errorf("output aliases input")
	}
	protected := []string{in.Site.Management.KubeconfigRef.File}
	for _, c := range in.Site.Credentials {
		if c.PasswordRef.File != nil {
			protected = append(protected, *c.PasswordRef.File)
		}
	}
	if t := in.Site.Discovery.TLS; t != nil && t.CAFile != nil {
		protected = append(protected, *t.CAFile)
	}
	for _, e := range append(append([]config.Endpoint{}, in.Site.Discovery.DPUs...), in.Site.Discovery.Hosts...) {
		if e.TLS != nil && e.TLS.CAFile != nil {
			protected = append(protected, *e.TLS.CAFile)
		}
	}
	kubeconfigPath := in.Site.Management.KubeconfigRef.File
	if !filepath.IsAbs(kubeconfigPath) {
		kubeconfigPath = filepath.Join(filepath.Dir(input), kubeconfigPath)
	}
	if rawBytes, readErr := config.ReadReference(kubeconfigPath, 16<<20); readErr == nil {
		if raw, err := clientcmd.Load(rawBytes); err == nil {
			for _, auth := range raw.AuthInfos {
				for _, p := range []string{auth.ClientKey, auth.ClientCertificate, auth.TokenFile} {
					if p != "" {
						if !filepath.IsAbs(p) {
							p = filepath.Join(filepath.Dir(kubeconfigPath), p)
						}
						protected = append(protected, p)
					}
				}
			}
			for _, cluster := range raw.Clusters {
				p := cluster.CertificateAuthority
				if p != "" {
					if !filepath.IsAbs(p) {
						p = filepath.Join(filepath.Dir(kubeconfigPath), p)
					}
					protected = append(protected, p)
				}
			}
		}
	}
	for _, p := range protected {
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(input), p)
		}
		abs, _ := filepath.Abs(p)
		st, _ := os.Stat(abs)
		if abs == output || outputInfo != nil && st != nil && os.SameFile(st, outputInfo) {
			return nil, fmt.Errorf("output aliases a private reference")
		}
	}
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return nil, err
	}
	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(entropy)
	runDir := filepath.Join(filepath.Dir(output), trimExtension(filepath.Base(output))+".discovery", runID)
	if err := safePath(filepath.Join(runDir, "reserved")); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return nil, err
	}
	store := &Store{Output: output, RunDir: runDir, Input: in, Initial: initial}
	if initial.Exists {
		old, e := config.Decode(initial.Data, output)
		if e == nil {
			store.PriorInventory = old.Site.Inventory
		}
	}
	return store, nil
}
func (s *Store) RunID() string      { return filepath.Base(s.RunDir) }
func trimExtension(s string) string { return s[:len(s)-len(filepath.Ext(s))] }
func sameRequested(a, b config.Site) bool {
	key := func(s config.Site) string {
		targets := []inventory.ScopeEndpoint{}
		for _, group := range []struct {
			role string
			eps  []config.Endpoint
		}{{"dpus", s.Discovery.DPUs}, {"hosts", s.Discovery.Hosts}} {
			for _, e := range group.eps {
				origin, err := config.Origin(e.BMCEndpoint)
				if err != nil {
					return ""
				}
				selector := ""
				if e.SystemURI != nil {
					selector = *e.SystemURI
				}
				targets = append(targets, inventory.ScopeEndpoint{Role: group.role, Endpoint: origin, SystemURI: selector})
			}
		}
		slices.SortFunc(targets, func(a, b inventory.ScopeEndpoint) int {
			return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
		})
		return inventory.Digest("requested", targets)
	}
	return key(a) == key(b)
}
func safePath(p string) error {
	for current := p; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("output path contains a symlink")
			}
			if current == p && !st.Mode().IsRegular() {
				return fmt.Errorf("output must be a regular file")
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("cannot inspect output path")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func snapshot(p string) (Snapshot, error) {
	st, err := os.Stat(p)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > 16<<20 {
		return Snapshot{}, fmt.Errorf("cannot inspect bounded output file")
	}
	b, err := os.ReadFile(p)
	return Snapshot{Exists: true, Data: b}, err
}
func (s *Store) Publish(ctx context.Context, report *inventory.Report) error {
	if report == nil {
		return fmt.Errorf("nil report")
	}
	lockPath := s.Output + ".lock"
	if err := safePath(lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	acquired, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !acquired {
		return fmt.Errorf("cannot acquire discovery output lock")
	}
	defer lock.Unlock()
	source, err := os.ReadFile(s.Input.Path)
	if err != nil || !bytes.Equal(source, s.Input.Source) {
		return fmt.Errorf("source changed during discovery")
	}
	current, err := snapshot(s.Output)
	if err != nil || current.Exists != s.Initial.Exists || !bytes.Equal(current.Data, s.Initial.Data) {
		return fmt.Errorf("output changed during discovery")
	}
	if err := safePath(s.Output); err != nil {
		return err
	}
	if report.Inventory != nil && report.Discovery.Outcome == "complete" && s.PriorInventory != nil {
		if s.PriorInventory.ScopeDigest != "" && s.PriorInventory.ScopeDigest != report.Inventory.ScopeDigest || inventory.IdentityConflict(s.PriorInventory, report.Inventory) {
			return fmt.Errorf("existing destination inventory conflicts with resolved scope or physical identity")
		}
	}
	if report.Inventory != nil {
		dest := s.Output
		if report.Discovery.Outcome != "complete" {
			dest = filepath.Join(s.RunDir, "partial.yaml")
		}
		encoded, err := config.Saved(s.Input, report.Inventory, dest)
		if err != nil {
			return err
		}
		if dest == s.Input.Path {
			backup := filepath.Join(s.RunDir, "source-before.yaml")
			if err := AtomicWrite(backup, s.Input.Source); err != nil {
				return err
			}
			report.Artifacts = append(report.Artifacts, inventory.Artifact{Path: backup, Kind: "source-backup"})
		}
		if err := AtomicWrite(dest, encoded); err != nil {
			return err
		}
		kind := "inventory"
		if dest != s.Output {
			kind = "partial-inventory"
		}
		report.Artifacts = append(report.Artifacts, inventory.Artifact{Path: dest, Kind: kind})
	}
	return s.Report(report)
}
func (s *Store) Report(report *inventory.Report) error {
	p := filepath.Join(s.RunDir, "report.json")
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := AtomicWrite(p, append(b, '\n')); err != nil {
		return err
	}
	report.Artifacts = append(report.Artifacts, inventory.Artifact{Path: p, Kind: "report"})
	return nil
}

// AtomicWrite uses restrictive same-directory temp files. Locks protect cooperating writers only.
func AtomicWrite(filename string, data []byte) error {
	if err := safePath(filename); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".l8k-dpf-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return replaceFile(name, filename)
	}
	if err := os.Rename(name, filename); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
