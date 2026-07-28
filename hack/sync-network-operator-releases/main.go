// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	defaultCatalogPath = "pkg/networkoperatorplugin/releases/releases.yaml"
	defaultAPIBaseURL  = "https://api.github.com"
	commandTimeout     = 2 * time.Minute
	httpTimeout        = 30 * time.Second
)

func main() {
	catalogPath := flag.String(
		"catalog",
		defaultCatalogPath,
		"path to the k8s-launch-kit Network Operator release catalog",
	)
	apiBaseURL := flag.String(
		"api-base-url",
		defaultAPIBaseURL,
		"GitHub API base URL",
	)
	printLatestTag := flag.Bool(
		"print-latest-tag",
		false,
		"print the Network Operator tag from the highest catalog release and exit",
	)
	newerThan := flag.String(
		"newer-than",
		"",
		"require the tag printed by --print-latest-tag to be newer than this tag",
	)
	flag.Parse()

	if *printLatestTag {
		catalog, err := readCatalog(*catalogPath)
		if err != nil {
			exitWithError(err)
		}
		tag, err := latestCatalogTag(catalog)
		if err != nil {
			exitWithError(err)
		}
		if *newerThan != "" {
			if err := requireNewerTag(tag, *newerThan); err != nil {
				exitWithError(err)
			}
		}
		fmt.Println(tag)
		return
	}
	if *newerThan != "" {
		exitWithError(errors.New("--newer-than requires --print-latest-tag"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	result, err := syncCatalog(ctx, syncOptions{
		CatalogPath: *catalogPath,
		APIBaseURL:  *apiBaseURL,
		Token:       os.Getenv("GITHUB_TOKEN"),
		HTTPClient:  &http.Client{Timeout: httpTimeout},
	})
	if err != nil {
		exitWithError(fmt.Errorf("sync Network Operator releases: %w", err))
	}

	if !result.Changed {
		fmt.Println("Network Operator release catalog is already current")
		return
	}

	for _, update := range result.Updates {
		fmt.Printf(
			"updated %s from %s: Network Operator %s, DOCA driver %s\n",
			update.Release,
			update.Ref,
			update.NetworkOperatorVersion,
			update.DOCADriverVersion,
		)
	}
}

func readCatalog(path string) ([]byte, error) {
	if path == "-" {
		catalog, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read catalog from stdin: %w", err)
		}
		return catalog, nil
	}

	catalog, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %q: %w", path, err)
	}
	return catalog, nil
}

func exitWithError(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
