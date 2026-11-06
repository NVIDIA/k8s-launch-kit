// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
)

// Publish writes a complete new directory on Linux/macOS only. An existing
// byte-identical complete bundle is unchanged; every other destination fails.
// The sibling lock coordinates Launch Kit writers. No-replace rename also
// protects a destination created concurrently by a noncooperating writer.
func (b *Bundle) Publish(ctx context.Context, dest string) (Publication, error) {
	if b == nil {
		return Publication{}, fmt.Errorf("cannot publish a nil DPF bundle")
	}
	if ctx == nil {
		return Publication{}, fmt.Errorf("DPF publication context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Publication{}, err
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return Publication{}, fmt.Errorf("DPF directory publication is unsupported on %s; owner-only permissions and no-replace rename are qualified on Linux/macOS only", runtime.GOOS)
	}
	if dest == "" {
		return Publication{}, fmt.Errorf("DPF publication destination is empty")
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return Publication{}, err
	}
	if err := noSymlinkPath(abs); err != nil {
		return Publication{}, err
	}
	parent := filepath.Dir(abs)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return Publication{}, fmt.Errorf("DPF publication parent must already be a directory")
	}
	lockPath := abs + ".lock"
	if err := noSymlinkPath(lockPath); err != nil {
		return Publication{}, err
	}
	if existing, err := os.Lstat(lockPath); err == nil && !existing.Mode().IsRegular() {
		return Publication{}, fmt.Errorf("DPF publication lock is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return Publication{}, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Publication{}, fmt.Errorf("create DPF publication lock: %w", err)
	}
	if err := lockFile.Close(); err != nil {
		return Publication{}, err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return Publication{}, fmt.Errorf("acquire DPF publication lock: %w", err)
	}
	if !locked {
		return Publication{}, fmt.Errorf("acquire DPF publication lock: %w", ctx.Err())
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return Publication{}, err
	}
	if err := noSymlinkPath(abs); err != nil {
		return Publication{}, err
	}
	if _, err := os.Lstat(abs); err == nil {
		existing, err := Load(abs)
		if err != nil {
			return Publication{}, fmt.Errorf("existing DPF destination is not a complete bundle: %w", err)
		}
		if !sameBytes(b, existing) {
			return Publication{}, fmt.Errorf("existing DPF destination differs; choose a new directory")
		}
		return Publication{Path: abs, Unchanged: true}, nil
	} else if !os.IsNotExist(err) {
		return Publication{}, err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(abs)+".staging-")
	if err != nil {
		return Publication{}, err
	}
	defer os.RemoveAll(staging)
	if err := b.writeStaging(ctx, staging); err != nil {
		return Publication{}, err
	}
	verified, err := Load(staging)
	if err != nil {
		return Publication{}, fmt.Errorf("verify staged DPF bundle: %w", err)
	}
	if !sameBytes(b, verified) {
		return Publication{}, fmt.Errorf("staged DPF bundle differs from retained snapshot")
	}
	if err := ctx.Err(); err != nil {
		return Publication{}, err
	}
	if err := noSymlinkPath(parent); err != nil {
		return Publication{}, err
	}
	if err := renameNewDirectory(staging, abs); err != nil {
		return Publication{}, fmt.Errorf("publish DPF directory without replacing destination: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return Publication{}, fmt.Errorf("sync published DPF parent: %w", err)
	}
	return Publication{Path: abs}, nil
}

func (b *Bundle) writeStaging(ctx context.Context, dir string) error {
	for _, file := range b.persistedFiles() {
		if err := ctx.Err(); err != nil {
			return err
		}
		filename := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return err
		}
		output, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := output.Write(file.Data)
		if writeErr == nil {
			writeErr = output.Sync()
		}
		closeErr := output.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	var directories []string
	if err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		directories = append(directories, name)
		return nil
	}); err != nil {
		return err
	}
	// Sync directories bottom-up before making the root visible.
	for n := len(directories) - 1; n >= 0; n-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDirectory(directories[n]); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(name string) error {
	directory, err := os.Open(name)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
