// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const maxFileSize = 16 * 1024 // 16KB truncation limit (~4K tokens)

// ExecuteCollectSosreport runs the sosreport script and returns the path to the unpacked directory.
// The script writes diagnostic data directly into the --output-dir path.
// If targetDir is non-empty, data is collected there; otherwise a temp directory is used.
func ExecuteCollectSosreport(scriptPath, kubeconfig, namespace, targetDir string) (string, error) {
	parentDir := targetDir
	if parentDir == "" {
		var err error
		parentDir, err = os.MkdirTemp("", "l8k-sosreport-*")
		if err != nil {
			return "", fmt.Errorf("failed to create temp directory: %w", err)
		}
	} else {
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create output directory %s: %w", parentDir, err)
		}
	}

	// Use --no-compress so we get the directory directly (no need to extract a tarball)
	args := []string{
		scriptPath,
		"--kubeconfig", kubeconfig,
		"--output-dir", parentDir,
		"--no-compress",
	}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}

	log.Log.V(2).Info("Running sosreport script", "script", scriptPath, "args", args)

	cmd := exec.Command("bash", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sosreport script failed: %w\nOutput: %s", err, string(output))
	}

	log.Log.V(2).Info("Sosreport script completed")

	// The script creates the data directly in the output-dir path
	// Check if it has sosreport content (diagnostic-summary.txt is a good indicator)
	if _, err := os.Stat(filepath.Join(parentDir, "diagnostic-summary.txt")); err == nil {
		return parentDir, nil
	}

	// Some versions create a timestamped subdirectory inside the output dir
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return "", fmt.Errorf("failed to read output directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			subdir := filepath.Join(parentDir, entry.Name())
			if _, err := os.Stat(filepath.Join(subdir, "diagnostic-summary.txt")); err == nil {
				return subdir, nil
			}
		}
	}

	// Fallback: return the parent dir if it has any content
	if len(entries) > 0 {
		return parentDir, nil
	}

	return "", fmt.Errorf("no sosreport output found in %s", parentDir)
}

// ExecuteReadFile reads a file or lists a directory from the sosreport.
// The path must be within sosreportBaseDir to prevent directory traversal.
// Returns the file contents or directory listing as a string.
func ExecuteReadFile(path, sosreportBaseDir string) string {
	// Resolve to absolute and validate the path is within the base dir
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Sprintf("Error: invalid path: %v", err)
	}
	absBase, err := filepath.Abs(sosreportBaseDir)
	if err != nil {
		return fmt.Sprintf("Error: invalid base path: %v", err)
	}

	if !strings.HasPrefix(absPath, absBase+string(filepath.Separator)) && absPath != absBase {
		return fmt.Sprintf("Error: path %q is outside the sosreport directory %q", path, sosreportBaseDir)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("Error: path does not exist: %s", absPath)
		}
		return fmt.Sprintf("Error: cannot access path: %v", err)
	}

	if info.IsDir() {
		return listDirectory(absPath)
	}

	return readFileContent(absPath, info.Size())
}

func listDirectory(dirPath string) string {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return fmt.Sprintf("Error: cannot list directory: %v", err)
	}

	if len(entries) == 0 {
		return fmt.Sprintf("Directory %s is empty", dirPath)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Directory listing of %s:\n", dirPath))
	for _, entry := range entries {
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		info, err := entry.Info()
		if err != nil {
			sb.WriteString(fmt.Sprintf("  %s%s\n", entry.Name(), suffix))
			continue
		}
		sb.WriteString(fmt.Sprintf("  %s%s (%d bytes)\n", entry.Name(), suffix, info.Size()))
	}
	return sb.String()
}

func readFileContent(filePath string, size int64) string {
	if size > maxFileSize {
		data := make([]byte, maxFileSize)
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Sprintf("Error: cannot open file: %v", err)
		}
		defer f.Close()
		n, err := f.Read(data)
		if err != nil {
			return fmt.Sprintf("Error: cannot read file: %v", err)
		}
		return fmt.Sprintf("%s\n\n[Truncated: showing first %d bytes of %d total]", string(data[:n]), n, size)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Sprintf("Error: cannot read file: %v", err)
	}
	return string(data)
}
