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

package presets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- DownloadPresets tests ---

func TestDownloadPresets_Success(t *testing.T) {
	// Set up a mock GitHub API server
	mux := http.NewServeMux()

	// Mock the contents API listing
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("expected ref=main, got %q", r.URL.Query().Get("ref"))
		}
		entries := []githubEntry{
			{Name: "MachineA", Type: "dir"},
			{Name: "MachineB", Type: "dir"},
			{Name: "README.md", Type: "file"}, // should be skipped
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})

	// Mock raw content downloads
	mux.HandleFunc("/test/repo/main/presets/MachineA/topology.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("machineType: MachineA\npfs: []\n"))
	})
	mux.HandleFunc("/test/repo/main/presets/MachineB/topology.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("machineType: MachineB\npfs:\n  - deviceID: a2dc\n    pciAddress: \"0000:1a:00.0\"\n"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	destDir := filepath.Join(t.TempDir(), "presets")

	// Patch URLs to use test server — create a custom download function
	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        destDir,
		HTTPClient: server.Client(),
	}

	// Override the download to use our test server URLs
	downloaded, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sort.Strings(downloaded)
	if len(downloaded) != 2 {
		t.Fatalf("expected 2 downloads, got %d: %v", len(downloaded), downloaded)
	}
	if downloaded[0] != "MachineA" || downloaded[1] != "MachineB" {
		t.Errorf("unexpected downloads: %v", downloaded)
	}

	// Verify files were written
	dataA, err := os.ReadFile(filepath.Join(destDir, "MachineA", "topology.yaml"))
	if err != nil {
		t.Fatalf("failed to read MachineA: %v", err)
	}
	if !strings.Contains(string(dataA), "machineType: MachineA") {
		t.Errorf("MachineA content mismatch: %s", string(dataA))
	}

	dataB, err := os.ReadFile(filepath.Join(destDir, "MachineB", "topology.yaml"))
	if err != nil {
		t.Fatalf("failed to read MachineB: %v", err)
	}
	if !strings.Contains(string(dataB), "machineType: MachineB") {
		t.Errorf("MachineB content mismatch: %s", string(dataB))
	}
}

func TestDownloadPresets_EmptyRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubEntry{})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	downloaded, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(downloaded) != 0 {
		t.Errorf("expected 0 downloads, got %d", len(downloaded))
	}
}

func TestDownloadPresets_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got: %v", err)
	}
}

func TestDownloadPresets_FileDownloadError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		entries := []githubEntry{
			{Name: "MachineA", Type: "dir"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	// No handler for the raw content — will 404

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err == nil {
		t.Fatal("expected error for missing topology.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "MachineA") {
		t.Errorf("expected MachineA in error, got: %v", err)
	}
}

func TestDownloadPresets_CustomBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		if ref != "develop" {
			t.Errorf("expected ref=develop, got %q", ref)
		}
		entries := []githubEntry{
			{Name: "TestMachine", Type: "dir"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	mux.HandleFunc("/test/repo/develop/presets/TestMachine/topology.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("machineType: TestMachine\n"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "develop",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	downloaded, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(downloaded) != 1 || downloaded[0] != "TestMachine" {
		t.Errorf("expected [TestMachine], got %v", downloaded)
	}
}

func TestDownloadPresets_OverwritesExisting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		entries := []githubEntry{
			{Name: "MachineA", Type: "dir"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	mux.HandleFunc("/test/repo/main/presets/MachineA/topology.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("machineType: MachineA\ngpuType: UPDATED\n"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	destDir := t.TempDir()

	// Create an existing preset that should be overwritten
	existingDir := filepath.Join(destDir, "MachineA")
	_ = os.MkdirAll(existingDir, 0o755)
	_ = os.WriteFile(filepath.Join(existingDir, "topology.yaml"), []byte("machineType: MachineA\ngpuType: OLD\n"), 0o644)

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        destDir,
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(existingDir, "topology.yaml"))
	if !strings.Contains(string(data), "UPDATED") {
		t.Errorf("expected file to be overwritten with UPDATED, got: %s", string(data))
	}
}

func TestDownloadPresets_OnlyDownloadsDirs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		entries := []githubEntry{
			{Name: "README.md", Type: "file"},
			{Name: ".gitkeep", Type: "file"},
			{Name: "ValidMachine", Type: "dir"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	mux.HandleFunc("/test/repo/main/presets/ValidMachine/topology.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("machineType: ValidMachine\n"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	downloaded, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(downloaded) != 1 || downloaded[0] != "ValidMachine" {
		t.Errorf("expected only ValidMachine, got %v", downloaded)
	}
}

func TestDownloadPresets_GithubTokenHeader(t *testing.T) {
	tokenSeen := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth == "Bearer test-token-123" {
			tokenSeen = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubEntry{})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	// Set GITHUB_TOKEN
	t.Setenv("GITHUB_TOKEN", "test-token-123")

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tokenSeen {
		t.Error("expected Authorization header with GITHUB_TOKEN, but it was not sent")
	}
}

func TestDownloadPresets_InvalidJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not valid json"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(context.Background(), opts, server.URL)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestDownloadPresets_ContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/contents/presets", func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response — context should cancel before this completes
		<-r.Context().Done()
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	opts := DownloadOptions{
		Repo:       "test/repo",
		Branch:     "main",
		Dir:        t.TempDir(),
		HTTPClient: server.Client(),
	}

	_, err := downloadPresetsWithBaseURL(ctx, opts, server.URL)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestDownloadPresets_DefaultOptions(t *testing.T) {
	opts := DefaultDownloadOptions()
	if opts.Repo != "nvidia/k8s-launch-kit" {
		t.Errorf("expected default repo nvidia/k8s-launch-kit, got %q", opts.Repo)
	}
	if opts.Branch != "main" {
		t.Errorf("expected default branch main, got %q", opts.Branch)
	}
	if opts.Dir != "" {
		t.Errorf("expected empty default dir, got %q", opts.Dir)
	}
}

// --- listGitHubDir tests ---

func TestListGitHubDir_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test-endpoint", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.v3+json" {
			t.Errorf("expected GitHub API Accept header, got %q", r.Header.Get("Accept"))
		}
		entries := []githubEntry{
			{Name: "dir1", Type: "dir"},
			{Name: "file1.txt", Type: "file"},
		}
		_ = json.NewEncoder(w).Encode(entries)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	entries, err := listGitHubDir(context.Background(), server.Client(), server.URL+"/test-endpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "dir1" || entries[0].Type != "dir" {
		t.Errorf("unexpected entry[0]: %+v", entries[0])
	}
}

func TestListGitHubDir_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test-endpoint", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := listGitHubDir(context.Background(), server.Client(), server.URL+"/test-endpoint")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestListGitHubDir_RateLimited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test-endpoint", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "API rate limit exceeded"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := listGitHubDir(context.Background(), server.Client(), server.URL+"/test-endpoint")
	if err == nil {
		t.Fatal("expected error for rate limit, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 in error, got: %v", err)
	}
}

// --- fetchURL tests ---

func TestFetchURL_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test-file", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("file content here"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	data, err := fetchURL(context.Background(), server.Client(), server.URL+"/test-file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "file content here" {
		t.Errorf("expected 'file content here', got %q", string(data))
	}
}

func TestFetchURL_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := fetchURL(context.Background(), server.Client(), server.URL+"/missing")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got: %v", err)
	}
}

func TestFetchURL_WithGithubToken(t *testing.T) {
	tokenSeen := false
	mux := http.NewServeMux()
	mux.HandleFunc("/test-file", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer my-token" {
			tokenSeen = true
		}
		_, _ = w.Write([]byte("ok"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", "my-token")

	_, err := fetchURL(context.Background(), server.Client(), server.URL+"/test-file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tokenSeen {
		t.Error("expected Authorization header, not found")
	}
}
