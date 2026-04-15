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

package fwresolver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

const (
	baseURL     = "https://www.mellanox.com"
	apiPath     = "/downloads/firmware/mlx_fw_online_query.php"
	httpTimeout = 30 * time.Second

	// DefaultAPIKey is the default key used by mlxfwmanager (mstflint cmd_line_params.cpp).
	// It enables the public firmware catalog without requiring user configuration.
	DefaultAPIKey = "last_release"
)

// psidQueryRequest is the JSON body for mode 0 (update check) of the Mellanox firmware API.
// This mirrors the mstflint updateMFAsRequest wire format.
// devs is always empty (the C++ also ignores dev_types_list).
type psidQueryRequest struct {
	PSIDs    string `json:"psids"`
	Devs     string `json:"devs"`
	Mode     int    `json:"mode"`
	Versions string `json:"versions"`
	Key      string `json:"key"`
}

// psidQueryItem represents one entry in the API response array.
type psidQueryItem struct {
	Found int    `json:"Found"`
	PSID  string `json:"PSID"`
	URL   string `json:"URL"`
}

// PSIDInfo holds a PSID and the currently installed firmware version for that device.
type PSIDInfo struct {
	PSID             string
	CurrentFwVersion string
}

// ResolveURLs queries the Mellanox firmware API (mode 0 / update-check) for the given
// PSID+version pairs and returns a map of PSID → full download URL for all found PSIDs.
// PSIDs with no result are silently omitted.
// Non-fatal: logs a warning on network/parse error and returns an empty map.
func ResolveURLs(ctx context.Context, logger logr.Logger, psidInfos []PSIDInfo, httpClient ...*http.Client) (map[string]string, error) {
	if len(psidInfos) == 0 {
		return nil, nil
	}

	client := &http.Client{Timeout: httpTimeout}
	if len(httpClient) > 0 && httpClient[0] != nil {
		client = httpClient[0]
	}

	psids := make([]string, len(psidInfos))
	versions := make([]string, len(psidInfos))
	for i, info := range psidInfos {
		psids[i] = info.PSID
		versions[i] = info.CurrentFwVersion
	}

	reqBody := psidQueryRequest{
		PSIDs:    strings.Join(psids, ","),
		Devs:     "",
		Mode:     0,
		Versions: strings.Join(versions, ","),
		Key:      DefaultAPIKey,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal firmware query request: %w", err)
	}

	endpoint := baseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create firmware query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	logger.Info("Querying Mellanox firmware API", "psids", psids)
	logger.V(1).Info("Equivalent curl command",
		"curl", fmt.Sprintf("curl -s -X POST %s -H 'Content-Type: application/json' -d '%s'", endpoint, string(bodyBytes)))
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firmware API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read firmware API response: %w", err)
	}

	logger.V(1).Info("Firmware API raw response", "status", resp.StatusCode, "body", string(respBytes))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firmware API returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var items []psidQueryItem
	if err := json.Unmarshal(respBytes, &items); err != nil {
		return nil, fmt.Errorf("failed to parse firmware API response: %w", err)
	}

	result := make(map[string]string, len(items))
	for _, item := range items {
		if item.Found != 1 || item.PSID == "" || item.URL == "" {
			continue
		}
		result[item.PSID] = baseURL + "/" + item.URL
	}

	logger.Info("Resolved firmware URLs", "count", len(result))
	return result, nil
}
