// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"bufio"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"
)

type publicationError struct {
	message string
	cause   error
}

func (e *publicationError) Error() string { return e.message }
func (e *publicationError) Unwrap() error { return e.cause }

// ReportDestination holds the directory identity checked before observation.
// The caller closes it after publishing or abandoning the report.
type ReportDestination struct {
	parent    reportParent
	filename  string
	bundle    reportInput
	protected []reportInput
}

type reportParent struct {
	root     *os.Root
	path     string
	resolved string
	info     fs.FileInfo
}

type reportInput struct {
	path     string
	resolved string
	info     fs.FileInfo
	parent   fs.FileInfo
}

// PrepareReportDestination accepts a disabled path ("-"), an explicit file in
// an existing directory, or an empty path for a new private sibling of the
// retained bundle. Protected paths include inventory, overrides, and access
// references resolved by the caller.
func PrepareReportDestination(bundlePath, reportPath string, protectedPaths []string) (*ReportDestination, error) {
	destination := &ReportDestination{}
	if reportPath == "-" {
		return destination, nil
	}
	if bundlePath == "" {
		return nil, &publicationError{message: "A deployment bundle path is required to prepare the report destination"}
	}
	bundle, err := inspectReportInput(bundlePath)
	if err != nil {
		return nil, &publicationError{"Cannot verify the deployment bundle path for report publication", err}
	}
	destination.bundle = bundle
	for _, path := range protectedPaths {
		if path == "" {
			continue
		}
		input, err := inspectReportInput(path)
		if err != nil {
			return nil, &publicationError{"Cannot verify a protected input path for report publication", err}
		}
		destination.protected = append(destination.protected, input)
	}

	if reportPath == "" {
		if err := destination.prepareDefault(); err != nil {
			return nil, err
		}
	} else {
		path, err := filepath.Abs(reportPath)
		if err != nil {
			return nil, &publicationError{"Cannot resolve the report path", err}
		}
		if os.IsPathSeparator(reportPath[len(reportPath)-1]) {
			return nil, &publicationError{message: "Choose a report file path within an existing directory"}
		}
		parent, err := openReportParent(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		destination.parent, destination.filename = parent, filepath.Base(path)
	}
	if err := destination.check(); err != nil {
		_ = destination.Close()
		return nil, err
	}
	return destination, nil
}

func (d *ReportDestination) prepareDefault() error {
	parent, err := openReportParent(filepath.Dir(d.bundle.resolved))
	if err != nil {
		return err
	}
	defer parent.root.Close()
	if err := parent.check(d.bundle); err != nil {
		return err
	}
	name := filepath.Base(d.bundle.resolved) + "-validation-" + rand.Text()
	if err := parent.root.Mkdir(name, 0o700); err != nil {
		return &publicationError{"Cannot reserve an adjacent report directory", err}
	}
	path := filepath.Join(parent.path, name)
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		return &publicationError{"Cannot open the adjacent report directory", err}
	}
	child, err := pinReportParent(root, path)
	if err != nil {
		return err
	}
	d.parent, d.filename = child, "validation.html"
	return nil
}

func openReportParent(path string) (reportParent, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return reportParent{}, &publicationError{"The report parent directory must already exist and be accessible. Create it or choose another directory.", err}
	}
	return pinReportParent(root, path)
}

func pinReportParent(root *os.Root, path string) (reportParent, error) {
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return reportParent{}, &publicationError{"Cannot inspect the report parent directory", err}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		_ = root.Close()
		return reportParent{}, &publicationError{"Cannot resolve the report parent directory", err}
	}
	return reportParent{root: root, path: path, resolved: resolved, info: info}, nil
}

func (p reportParent) check(bundle reportInput) error {
	resolved, err := filepath.EvalSymlinks(p.path)
	if err != nil || resolved != p.resolved {
		return &publicationError{"The report parent directory changed. Select a stable destination.", err}
	}
	current, err := os.Stat(p.path)
	if err != nil || !os.SameFile(p.info, current) {
		return &publicationError{"The report parent directory changed. Select a stable destination.", err}
	}
	currentBundle, err := inspectReportInput(bundle.path)
	if err != nil {
		return &publicationError{"Cannot verify the deployment bundle path for report publication", err}
	}
	for dir, info := resolved, current; ; dir = filepath.Dir(dir) {
		if sameReportPath(dir, bundle.resolved) || sameReportPath(dir, currentBundle.resolved) ||
			sameReportFile(info, bundle.info) || sameReportFile(info, currentBundle.info) {
			return &publicationError{message: "Choose a report destination outside the deployment bundle"}
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
		info, err = os.Stat(filepath.Dir(dir))
		if err != nil {
			return &publicationError{"Cannot verify the report parent directory ancestry", err}
		}
	}
}

func (d *ReportDestination) check() error {
	if err := d.parent.check(d.bundle); err != nil {
		return err
	}
	info, err := d.parent.root.Lstat(d.filename)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &publicationError{"Cannot inspect the report destination", err}
	}
	if info != nil && !info.Mode().IsRegular() {
		return &publicationError{message: "Choose a report destination that is a regular file or does not exist"}
	}
	path := filepath.Join(d.parent.resolved, d.filename)
	if err := checkProtectedReportInput(path, d.parent.info, info, d.bundle); err != nil {
		return err
	}
	for _, input := range d.protected {
		if err := checkProtectedReportInput(path, d.parent.info, info, input); err != nil {
			return err
		}
	}
	return nil
}

func checkProtectedReportInput(output string, parent, info fs.FileInfo, input reportInput) error {
	current, err := inspectReportInput(input.path)
	if err != nil {
		return &publicationError{"Cannot verify a protected input path for report publication", err}
	}
	if aliasesReportInput(output, parent, info, input) || aliasesReportInput(output, parent, info, current) {
		return &publicationError{message: "The report destination aliases a protected input file. Choose a different path."}
	}
	return nil
}

func aliasesReportInput(output string, parent, info fs.FileInfo, input reportInput) bool {
	return sameReportPath(output, input.resolved) || sameReportFile(info, input.info) ||
		(sameReportFile(parent, input.parent) && sameReportPath(filepath.Base(output), filepath.Base(input.resolved)))
}

// Compare missing paths conservatively on case-insensitive filesystems.
// Existing files and directories also get a device/inode comparison.
func sameReportPath(a, b string) bool {
	return strings.EqualFold(norm.NFC.String(a), norm.NFC.String(b))
}

func sameReportFile(a, b fs.FileInfo) bool { return a != nil && b != nil && os.SameFile(a, b) }

func inspectReportInput(path string) (reportInput, error) {
	absolute, err := absoluteReportInputPath(path)
	if err != nil {
		return reportInput{}, err
	}
	resolved, err := resolveReportInputPath(absolute, 0)
	if err != nil {
		return reportInput{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil && !missingReportInput(err) {
		return reportInput{}, err
	}
	parent, err := os.Stat(filepath.Dir(resolved))
	if err != nil && !missingReportInput(err) {
		return reportInput{}, err
	}
	return reportInput{path: absolute, resolved: resolved, info: info, parent: parent}, nil
}

func absoluteReportInputPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if filepath.VolumeName(path) != "" {
		return filepath.Abs(path)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// filepath.Abs cleans ".." before resolving earlier symlinks.
	return dir + string(filepath.Separator) + path, nil
}

// A source may be absent when preparation fails. Resolve its existing prefix
// and any dangling links so publishing cannot create a missing input by alias.
func resolveReportInputPath(path string, depth int) (string, error) {
	if depth == 255 {
		return "", errors.New("protected input path has too many components or symbolic links")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !missingReportInput(err) {
		return resolved, err
	}
	volume := filepath.VolumeName(path)
	path = strings.TrimRightFunc(path, pathSeparator)
	if path == volume {
		return "", err
	}
	dir, name := filepath.Split(path)
	if link, linkErr := os.Readlink(path); linkErr == nil {
		if !filepath.IsAbs(link) {
			// Preserve unresolved ".." components until symlinks in their
			// preceding path have been resolved.
			link = dir + link
		}
		return resolveReportInputPath(link, depth+1)
	}
	parent := strings.TrimRightFunc(dir, pathSeparator)
	if parent == volume {
		parent += string(filepath.Separator)
	}
	realParent, err := resolveReportInputPath(parent, depth+1)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, name), nil
}

func pathSeparator(r rune) bool { return r == '/' || r == filepath.Separator }

func missingReportInput(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// Close releases the pinned directory. A disabled destination needs no handle.
func (d *ReportDestination) Close() error {
	if d == nil || d.parent.root == nil {
		return nil
	}
	return d.parent.root.Close()
}

// Publish stages the rendered report in the pinned directory and replaces the
// destination only after a successful render and file sync. It can finalize
// using retained evidence even after the observation context is canceled.
func (d *ReportDestination) Publish(render func(io.Writer) error) (ReportDisposition, error) {
	if d == nil {
		return ReportDisposition{Status: ReportFailed}, &publicationError{message: "Report destination was not prepared"}
	}
	if d.parent.root == nil {
		return ReportDisposition{Status: ReportDisabled}, nil
	}
	path, err := d.publish(render)
	if err != nil {
		return ReportDisposition{Status: ReportFailed}, err
	}
	return ReportDisposition{Status: ReportPublished, Path: path}, nil
}

func (d *ReportDestination) publish(render func(io.Writer) error) (string, error) {
	if render == nil {
		return "", &publicationError{message: "Report renderer is unavailable"}
	}
	if err := d.check(); err != nil {
		return "", err
	}
	temp := ".l8k-validation-" + rand.Text()
	file, err := d.parent.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", &publicationError{"Cannot create the staged report", err}
	}
	tempInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = d.parent.root.Remove(temp)
		return "", &publicationError{"Cannot inspect the staged report", err}
	}
	defer func() {
		_ = file.Close()
		d.removeStagedReport(temp, tempInfo)
	}()
	writer := bufio.NewWriter(file)
	err = render(writer)
	if err == nil {
		err = writer.Flush()
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return "", &publicationError{"Cannot finish writing the staged report", err}
	}
	if err := d.check(); err != nil {
		return "", err
	}
	before, err := d.parent.root.Lstat(temp)
	if err != nil || !sameReportFile(tempInfo, before) {
		return "", &publicationError{"The staged report changed before publication", err}
	}
	if err := d.parent.root.Rename(temp, d.filename); err != nil {
		return "", &publicationError{"Cannot replace the report file", err}
	}

	if err := d.parent.check(d.bundle); err != nil {
		return "", err
	}
	path := filepath.Join(d.parent.path, d.filename)
	after, err := os.Lstat(path)
	if err != nil || !sameReportFile(tempInfo, after) {
		return "", &publicationError{"The published report path no longer identifies this report", err}
	}
	return path, nil
}

func (d *ReportDestination) removeStagedReport(name string, opened fs.FileInfo) {
	if current, err := d.parent.root.Lstat(name); err == nil && sameReportFile(current, opened) {
		_ = d.parent.root.Remove(name)
	}
}
