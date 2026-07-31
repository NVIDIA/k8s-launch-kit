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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

const (
	releaseBranchPrefix = "v"
	releaseBranchSuffix = ".x"
	fallbackBranch      = "master"
	upstreamRepository  = "Mellanox/network-operator"

	stableOperatorRepository  = "nvcr.io/nvidia/cloud-native"
	stableComponentRepository = "nvcr.io/nvidia/mellanox"
	stableHelmRepoURL         = "https://helm.ngc.nvidia.com/nvidia"

	stagingOperatorRepository  = "nvcr.io/nvstaging/mellanox"
	stagingComponentRepository = "nvcr.io/nvstaging/mellanox"
	stagingHelmRepoURL         = "https://helm.ngc.nvidia.com/nvstaging/mellanox"
)

type syncOptions struct {
	CatalogPath string
	APIBaseURL  string
	Token       string
	HTTPClient  *http.Client
}

type syncResult struct {
	Changed bool
	Updates []releaseUpdate
}

type releaseUpdate struct {
	Release                string
	Ref                    string
	NetworkOperatorVersion string
	DOCADriverVersion      string
}

type upstreamReleaseFile struct {
	NetworkOperator              upstreamImage `yaml:"NetworkOperator"`
	NetworkOperatorInitContainer upstreamImage `yaml:"NetworkOperatorInitContainer"`
	Mofed                        upstreamImage `yaml:"Mofed"`
}

type upstreamImage struct {
	Repository string `yaml:"repository"`
	Version    string `yaml:"version"`
}

type desiredRelease struct {
	NetworkOperatorVersion string
	ComponentVersion       string
	ComponentRepository    string
	OperatorRepository     string
	HelmRepoURL            string
	DOCADriverVersion      string
}

type desiredReleaseWithSource struct {
	Release string
	Ref     string
	Desired desiredRelease
}

type catalogRelease struct {
	Key     string
	Version *semver.Version
}

type githubClient struct {
	baseURL    string
	repository string
	token      string
	httpClient *http.Client
}

func syncCatalog(ctx context.Context, opts syncOptions) (syncResult, error) {
	if opts.CatalogPath == "" {
		return syncResult{}, errors.New("catalog path must not be empty")
	}
	if opts.APIBaseURL == "" {
		return syncResult{}, errors.New("GitHub API base URL must not be empty")
	}
	if opts.HTTPClient == nil {
		return syncResult{}, errors.New("HTTP client must not be nil")
	}

	original, err := os.ReadFile(opts.CatalogPath)
	if err != nil {
		return syncResult{}, fmt.Errorf("read catalog %q: %w", opts.CatalogPath, err)
	}

	managed, err := catalogReleases(original)
	if err != nil {
		return syncResult{}, err
	}

	client, err := newGitHubClient(
		opts.APIBaseURL,
		upstreamRepository,
		opts.Token,
		opts.HTTPClient,
	)
	if err != nil {
		return syncResult{}, err
	}

	desired := make([]desiredReleaseWithSource, 0, len(managed))
	latestRelease := managed[len(managed)-1].Key
	for _, catalogEntry := range managed {
		release := catalogEntry.Key
		ref, err := client.resolveRef(ctx, release, release == latestRelease)
		if err != nil {
			return syncResult{}, fmt.Errorf("resolve source for release %s: %w", release, err)
		}

		upstreamYAML, err := client.fetchReleaseFile(ctx, ref)
		if err != nil {
			return syncResult{}, fmt.Errorf("fetch release %s from %s: %w", release, ref, err)
		}

		releaseValues, err := desiredFromUpstream(release, upstreamYAML)
		if err != nil {
			return syncResult{}, fmt.Errorf("validate release %s from %s: %w", release, ref, err)
		}

		desired = append(desired, desiredReleaseWithSource{
			Release: release,
			Ref:     ref,
			Desired: releaseValues,
		})
	}

	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil {
		return syncResult{}, fmt.Errorf("parse catalog document: %w", err)
	}

	changedItems := make([]desiredReleaseWithSource, 0, len(desired))
	for _, item := range desired {
		itemChanged, err := applyDesiredRelease(&document, item.Release, item.Desired)
		if err != nil {
			return syncResult{}, err
		}
		if itemChanged {
			changedItems = append(changedItems, item)
		}
	}
	if len(changedItems) == 0 {
		return syncResult{Changed: false}, nil
	}

	var updated bytes.Buffer
	encoder := yaml.NewEncoder(&updated)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return syncResult{}, fmt.Errorf("encode updated catalog: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return syncResult{}, fmt.Errorf("close catalog encoder: %w", err)
	}

	if err := writeFileAtomically(opts.CatalogPath, updated.Bytes()); err != nil {
		return syncResult{}, err
	}

	result := syncResult{
		Changed: true,
		Updates: make([]releaseUpdate, 0, len(changedItems)),
	}
	for _, item := range changedItems {
		result.Updates = append(result.Updates, releaseUpdate{
			Release:                item.Release,
			Ref:                    item.Ref,
			NetworkOperatorVersion: item.Desired.NetworkOperatorVersion,
			DOCADriverVersion:      item.Desired.DOCADriverVersion,
		})
	}
	return result, nil
}

func catalogReleases(catalogYAML []byte) ([]catalogRelease, error) {
	var document struct {
		Releases map[string]yaml.Node `yaml:"releases"`
	}
	if err := yaml.Unmarshal(catalogYAML, &document); err != nil {
		return nil, fmt.Errorf("parse catalog releases: %w", err)
	}
	if len(document.Releases) == 0 {
		return nil, errors.New("catalog releases must not be empty")
	}

	releases := make([]catalogRelease, 0, len(document.Releases))
	for release := range document.Releases {
		version, err := semver.NewVersion(release + ".0")
		if err != nil {
			return nil, fmt.Errorf("catalog release %q is not MAJOR.MINOR: %w", release, err)
		}
		if fmt.Sprintf("%d.%d", version.Major(), version.Minor()) != release {
			return nil, fmt.Errorf("catalog release %q must use MAJOR.MINOR format", release)
		}
		releases = append(releases, catalogRelease{
			Key:     release,
			Version: version,
		})
	}

	sort.Slice(releases, func(i, j int) bool {
		return releases[i].Version.LessThan(releases[j].Version)
	})
	return releases, nil
}

func latestCatalogTag(catalogYAML []byte) (string, error) {
	releases, err := catalogReleases(catalogYAML)
	if err != nil {
		return "", err
	}
	latest := releases[len(releases)-1]

	var document struct {
		Releases map[string]struct {
			NetworkOperator struct {
				Version string `yaml:"version"`
			} `yaml:"networkOperator"`
		} `yaml:"releases"`
	}
	if err := yaml.Unmarshal(catalogYAML, &document); err != nil {
		return "", fmt.Errorf("parse catalog versions: %w", err)
	}

	entry, ok := document.Releases[latest.Key]
	if !ok {
		return "", fmt.Errorf("latest catalog release %q is missing", latest.Key)
	}
	if entry.NetworkOperator.Version == "" {
		return "", fmt.Errorf(
			"latest catalog release %q has no networkOperator.version",
			latest.Key,
		)
	}

	tagVersion, err := semver.NewVersion(entry.NetworkOperator.Version)
	if err != nil {
		return "", fmt.Errorf(
			"latest catalog tag %q is invalid: %w",
			entry.NetworkOperator.Version,
			err,
		)
	}
	if tagVersion.Major() != latest.Version.Major() ||
		tagVersion.Minor() != latest.Version.Minor() {
		return "", fmt.Errorf(
			"latest catalog tag %q does not belong to release line %s",
			entry.NetworkOperator.Version,
			latest.Key,
		)
	}
	return entry.NetworkOperator.Version, nil
}

func requireNewerTag(candidate string, previous string) error {
	candidateVersion, err := semver.NewVersion(candidate)
	if err != nil {
		return fmt.Errorf("candidate tag %q is invalid: %w", candidate, err)
	}
	previousVersion, err := semver.NewVersion(previous)
	if err != nil {
		return fmt.Errorf("previous tag %q is invalid: %w", previous, err)
	}
	if !candidateVersion.GreaterThan(previousVersion) {
		return fmt.Errorf(
			"candidate tag %q is not newer than %q",
			candidate,
			previous,
		)
	}
	return nil
}

func newGitHubClient(
	baseURL string,
	repository string,
	token string,
	httpClient *http.Client,
) (*githubClient, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("upstream repository %q must be OWNER/REPO", repository)
	}

	escapedRepository := url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	return &githubClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		repository: escapedRepository,
		token:      token,
		httpClient: httpClient,
	}, nil
}

func (c *githubClient) resolveRef(
	ctx context.Context,
	release string,
	allowMasterFallback bool,
) (string, error) {
	branch := releaseBranchPrefix + release + releaseBranchSuffix
	exists, err := c.branchExists(ctx, branch)
	if err != nil {
		return "", err
	}
	if exists {
		return branch, nil
	}
	if !allowMasterFallback {
		return "", fmt.Errorf("release branch %q does not exist", branch)
	}

	masterExists, err := c.branchExists(ctx, fallbackBranch)
	if err != nil {
		return "", err
	}
	if !masterExists {
		return "", fmt.Errorf(
			"release branch %q and fallback branch %q do not exist",
			branch,
			fallbackBranch,
		)
	}
	return fallbackBranch, nil
}

func (c *githubClient) branchExists(ctx context.Context, branch string) (bool, error) {
	endpoint := fmt.Sprintf(
		"%s/repos/%s/branches/%s",
		c.baseURL,
		c.repository,
		url.PathEscape(branch),
	)
	resp, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		_, _ = io.Copy(io.Discard, resp.Body)
		return true, nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	default:
		return false, githubStatusError(resp, endpoint)
	}
}

func (c *githubClient) fetchReleaseFile(ctx context.Context, ref string) ([]byte, error) {
	endpoint := fmt.Sprintf(
		"%s/repos/%s/contents/hack/release.yaml?ref=%s",
		c.baseURL,
		c.repository,
		url.QueryEscape(ref),
	)
	resp, err := c.get(ctx, endpoint, "application/vnd.github.raw+json")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, githubStatusError(resp, endpoint)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read GitHub response for %s: %w", endpoint, err)
	}
	return body, nil
}

func (c *githubClient) get(
	ctx context.Context,
	endpoint string,
	accept string,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", endpoint, err)
	}
	return response, nil
}

func githubStatusError(response *http.Response, endpoint string) error {
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if readErr != nil {
		return fmt.Errorf(
			"GitHub API returned %s for %s and the response body could not be read: %v",
			response.Status,
			endpoint,
			readErr,
		)
	}
	return fmt.Errorf(
		"GitHub API returned %s for %s: %s",
		response.Status,
		endpoint,
		strings.TrimSpace(string(body)),
	)
}

func desiredFromUpstream(release string, upstreamYAML []byte) (desiredRelease, error) {
	var upstream upstreamReleaseFile
	if err := yaml.Unmarshal(upstreamYAML, &upstream); err != nil {
		return desiredRelease{}, fmt.Errorf("parse hack/release.yaml: %w", err)
	}

	required := map[string]string{
		"NetworkOperator.version":                 upstream.NetworkOperator.Version,
		"NetworkOperator.repository":              upstream.NetworkOperator.Repository,
		"NetworkOperatorInitContainer.version":    upstream.NetworkOperatorInitContainer.Version,
		"NetworkOperatorInitContainer.repository": upstream.NetworkOperatorInitContainer.Repository,
		"Mofed.version":                           upstream.Mofed.Version,
		"Mofed.repository":                        upstream.Mofed.Repository,
	}
	for field, value := range required {
		if value == "" {
			return desiredRelease{}, fmt.Errorf("%s must not be empty", field)
		}
	}

	operatorVersion, err := semver.NewVersion(upstream.NetworkOperator.Version)
	if err != nil {
		return desiredRelease{}, fmt.Errorf(
			"upstream NetworkOperator.version %q is invalid: %w",
			upstream.NetworkOperator.Version,
			err,
		)
	}
	releaseVersion, err := semver.NewVersion(release + ".0")
	if err != nil {
		return desiredRelease{}, fmt.Errorf("release key %q is invalid: %w", release, err)
	}
	if operatorVersion.Major() != releaseVersion.Major() ||
		operatorVersion.Minor() != releaseVersion.Minor() {
		return desiredRelease{}, fmt.Errorf(
			"upstream NetworkOperator.version %q does not belong to release line %s",
			upstream.NetworkOperator.Version,
			release,
		)
	}

	expectedComponentVersion := "network-operator-" + upstream.NetworkOperator.Version
	if upstream.NetworkOperatorInitContainer.Version != expectedComponentVersion {
		return desiredRelease{}, fmt.Errorf(
			"upstream NetworkOperatorInitContainer.version %q does not match expected %q",
			upstream.NetworkOperatorInitContainer.Version,
			expectedComponentVersion,
		)
	}

	destination, err := artifactDestinationFor(upstream.NetworkOperator.Repository)
	if err != nil {
		return desiredRelease{}, err
	}
	if upstream.NetworkOperatorInitContainer.Repository != destination.ComponentRepository {
		return desiredRelease{}, fmt.Errorf(
			"upstream NetworkOperatorInitContainer.repository %q does not match expected %q",
			upstream.NetworkOperatorInitContainer.Repository,
			destination.ComponentRepository,
		)
	}
	if upstream.Mofed.Repository != destination.ComponentRepository {
		return desiredRelease{}, fmt.Errorf(
			"upstream Mofed.repository %q does not match expected %q",
			upstream.Mofed.Repository,
			destination.ComponentRepository,
		)
	}

	return desiredRelease{
		NetworkOperatorVersion: upstream.NetworkOperator.Version,
		ComponentVersion:       upstream.NetworkOperatorInitContainer.Version,
		ComponentRepository:    upstream.NetworkOperatorInitContainer.Repository,
		OperatorRepository:     upstream.NetworkOperator.Repository,
		HelmRepoURL:            destination.HelmRepoURL,
		DOCADriverVersion:      upstream.Mofed.Version,
	}, nil
}

type artifactDestination struct {
	ComponentRepository string
	HelmRepoURL         string
}

func artifactDestinationFor(operatorRepository string) (artifactDestination, error) {
	switch operatorRepository {
	case stableOperatorRepository:
		return artifactDestination{
			ComponentRepository: stableComponentRepository,
			HelmRepoURL:         stableHelmRepoURL,
		}, nil
	case stagingOperatorRepository:
		return artifactDestination{
			ComponentRepository: stagingComponentRepository,
			HelmRepoURL:         stagingHelmRepoURL,
		}, nil
	default:
		return artifactDestination{}, fmt.Errorf(
			"unsupported NetworkOperator.repository %q",
			operatorRepository,
		)
	}
}

func applyDesiredRelease(
	document *yaml.Node,
	release string,
	desired desiredRelease,
) (bool, error) {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return false, errors.New("catalog YAML has no document")
	}

	root := document.Content[0]
	releasesNode, err := mappingValue(root, "releases")
	if err != nil {
		return false, err
	}
	releaseNode, err := mappingValue(releasesNode, release)
	if err != nil {
		return false, fmt.Errorf("catalog release %s: %w", release, err)
	}
	networkOperatorNode, err := mappingValue(releaseNode, "networkOperator")
	if err != nil {
		return false, fmt.Errorf("catalog release %s: %w", release, err)
	}
	docaDriverNode, err := mappingValue(releaseNode, "docaDriver")
	if err != nil {
		return false, fmt.Errorf("catalog release %s: %w", release, err)
	}

	updates := []struct {
		parent *yaml.Node
		key    string
		value  string
	}{
		{networkOperatorNode, "version", desired.NetworkOperatorVersion},
		{networkOperatorNode, "componentVersion", desired.ComponentVersion},
		{networkOperatorNode, "repository", desired.ComponentRepository},
		{networkOperatorNode, "operatorRepository", desired.OperatorRepository},
		{networkOperatorNode, "helmRepoURL", desired.HelmRepoURL},
		{docaDriverNode, "version", desired.DOCADriverVersion},
	}

	changed := false
	for _, update := range updates {
		valueNode, err := mappingValue(update.parent, update.key)
		if err != nil {
			return false, fmt.Errorf("catalog release %s: %w", release, err)
		}
		if valueNode.Kind != yaml.ScalarNode {
			return false, fmt.Errorf(
				"catalog release %s field %s must be a scalar",
				release,
				update.key,
			)
		}
		if valueNode.Value != update.value {
			valueNode.Value = update.value
			changed = true
		}
	}
	return changed, nil
}

func mappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected YAML mapping while looking for %q", key)
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1], nil
		}
	}
	return nil, fmt.Errorf("missing YAML key %q", key)
}

func writeFileAtomically(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat catalog %q: %w", path, err)
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary catalog: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary catalog permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary catalog: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary catalog: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace catalog %q: %w", path, err)
	}
	removeTemporary = false
	return nil
}
