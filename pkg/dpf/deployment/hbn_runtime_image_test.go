// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type hbnImageTransportFunc func(*http.Request) (*http.Response, error)

func (f hbnImageTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
func imageMetadataDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
func imageMetadataJSON(value any) []byte     { data, _ := json.Marshal(value); return data }

func TestHBNRuntimeImageMetadataVerifiedWithoutAmbientCredentials(t *testing.T) {
	for _, scenario := range []string{"platform-manifest", "image-index", "same-origin-anonymous-bearer", "anonymous-config-redirect", "foreign-realm", "foreign-redirect", "wrong-manifest-digest", "wrong-config-digest", "wrong-platform", "ambiguous-platform", "duplicate-json", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			platform := "arm64"
			if scenario == "wrong-platform" {
				platform = "amd64"
			}
			config := imageMetadataJSON(map[string]any{"os": "linux", "architecture": platform})
			configDigest := imageMetadataDigest(config)
			manifest := imageMetadataJSON(map[string]any{"schemaVersion": 2, "mediaType": hbnManifestMediaType, "config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)}, "layers": []any{}})
			if scenario == "duplicate-json" {
				manifest = []byte(strings.Replace(string(manifest), `"schemaVersion":2`, `"schemaVersion":1,"schemaVersion":2`, 1))
			}
			manifestDigest := imageMetadataDigest(manifest)
			digest := manifestDigest
			root := manifest
			if scenario == "image-index" || scenario == "ambiguous-platform" {
				descriptor := map[string]any{"mediaType": hbnManifestMediaType, "digest": manifestDigest, "size": len(manifest), "platform": map[string]any{"os": "linux", "architecture": "arm64"}}
				descriptors := []any{descriptor}
				if scenario == "ambiguous-platform" {
					descriptors = append(descriptors, descriptor)
				}
				root = imageMetadataJSON(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": descriptors})
				digest = imageMetadataDigest(root)
			}
			reference := "registry.example/qualified/hbn:retained@" + digest
			requests, tokenRequests := 0, 0
			transport := hbnImageTransportFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if scenario == "anonymous-config-redirect" && request.URL.Host == "cdn.example" {
					for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
						if request.Header.Get(header) != "" {
							t.Fatalf("credential/header %s escaped to blob CDN", header)
						}
					}
					if request.URL.Scheme != "https" || request.URL.Path != "/immutable/config" {
						t.Fatal("unsafe blob redirect")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: int64(len(config)), Body: io.NopCloser(strings.NewReader(string(config))), Request: request}, nil
				}
				if request.URL.Scheme != "https" || request.URL.Host != "registry.example" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
					t.Fatal("metadata escaped explicit anonymous origin")
				}
				header := http.Header{}
				code := http.StatusOK
				data := root
				switch {
				case request.URL.Path == "/token":
					tokenRequests++
					if request.URL.Query().Get("scope") != "repository:qualified/hbn:pull" || request.Header.Get("Authorization") != "" {
						t.Fatal("token scope/credentials broadened")
					}
					data = []byte(`{"token":"anonymous-token"}`)
				case scenario == "foreign-realm":
					code = http.StatusUnauthorized
					header.Set("WWW-Authenticate", `Bearer realm="https://foreign.example/token",service="registry.example"`)
				case scenario == "foreign-redirect":
					code = http.StatusTemporaryRedirect
					header.Set("Location", "https://foreign.example/blob")
				case (scenario == "same-origin-anonymous-bearer" || scenario == "anonymous-config-redirect") && request.Header.Get("Authorization") == "":
					code = http.StatusUnauthorized
					header.Set("WWW-Authenticate", `Bearer realm="https://registry.example/token",service="registry.example",scope="repository:foreign:pull"`)
				case strings.Contains(request.URL.Path, "/blobs/"):
					if scenario == "anonymous-config-redirect" {
						code = http.StatusTemporaryRedirect
						header.Set("Location", "https://cdn.example/immutable/config?signature=private-test")
					}
					data = config
					if scenario == "wrong-config-digest" {
						data = []byte(`{"os":"linux","architecture":"foreign"}`)
					}
				case strings.HasSuffix(request.URL.Path, manifestDigest):
					data = manifest
				}
				if scenario == "wrong-manifest-digest" {
					data = []byte(`{}`)
				}
				if scenario == "oversized" {
					data = make([]byte, hbnImageMetadataLimit+1)
				}
				return &http.Response{StatusCode: code, Header: header, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(string(data))), Request: request}, nil
			})
			result, err := resolveHBNRuntimeImage(ctx, reference, time.Second, transport)
			success := scenario == "platform-manifest" || scenario == "image-index" || scenario == "same-origin-anonymous-bearer" || scenario == "anonymous-config-redirect"
			if success {
				if err != nil || result.Reference != reference || result.ManifestDigest != manifestDigest || result.ConfigDigest != configDigest {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil || result.Reference != "" {
				t.Fatalf("unsafe resolution: %+v %v", result, err)
			}
			if scenario == "same-origin-anonymous-bearer" && tokenRequests != 1 {
				t.Fatalf("token requests=%d", tokenRequests)
			}
			if scenario == "foreign-realm" && requests != 1 {
				t.Fatal("foreign authorization was followed")
			}
		})
	}
}
