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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveURLs_EmptyInput(t *testing.T) {
	urls, err := ResolveURLs(context.Background(), logr.Discard(), nil)
	require.NoError(t, err)
	assert.Nil(t, urls)
}

func TestResolveURLs_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var req psidQueryRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, 0, req.Mode)
		assert.Contains(t, req.PSIDs, "MT_0000000221")
		assert.Contains(t, req.Versions, "28.42.1000")

		resp := []psidQueryItem{
			{Found: 1, PSID: "MT_0000000221", URL: "downloads/fw/ConnectX8/latest.zip"},
			{Found: 0, PSID: "MT_0000000222", URL: ""},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := server.Client()
	client.Transport = &redirectTransport{base: server.URL, orig: client.Transport}

	infos := []PSIDInfo{
		{PSID: "MT_0000000221", CurrentFwVersion: "28.42.1000"},
		{PSID: "MT_0000000222", CurrentFwVersion: "28.42.1000"},
	}
	urls, err := ResolveURLs(context.Background(), logr.Discard(), infos, client)
	require.NoError(t, err)
	require.Len(t, urls, 1)
	assert.True(t, strings.HasSuffix(urls["MT_0000000221"], "downloads/fw/ConnectX8/latest.zip"))
	assert.NotContains(t, urls, "MT_0000000222")
}

func TestResolveURLs_NetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := server.Client()
	client.Transport = &redirectTransport{base: server.URL, orig: client.Transport}

	_, err := ResolveURLs(context.Background(), logr.Discard(), []PSIDInfo{{PSID: "MT_0000000221", CurrentFwVersion: "28.42.1000"}}, client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestResolveURLs_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-valid-json"))
	}))
	defer server.Close()

	client := server.Client()
	client.Transport = &redirectTransport{base: server.URL, orig: client.Transport}

	_, err := ResolveURLs(context.Background(), logr.Discard(), []PSIDInfo{{PSID: "MT_0000000221", CurrentFwVersion: "28.42.1000"}}, client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

// redirectTransport rewrites requests destined for the Mellanox base URL to the test server.
type redirectTransport struct {
	base string
	orig http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.base, "http://")
	if t.orig != nil {
		return t.orig.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}
