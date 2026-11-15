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
	"strconv"
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
	const quotedServiceAudience = `registry,region;tenant:/?&=+"quoted"`
	for _, scenario := range []string{"platform-manifest", "image-index", "same-origin-anonymous-bearer", "opaque-service-audience", "quoted-opaque-service-audience", "empty-service-audience", "whitespace-service-audience", "nonascii-service-audience", "oversized-service-audience", "malformed-challenge", "blob-path-token-redirect", "anonymous-config-redirect", "foreign-realm", "foreign-redirect", "wrong-manifest-digest", "wrong-config-digest", "wrong-platform", "ambiguous-platform", "duplicate-json", "oversized"} {
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
			requests, tokenRequests, foreignRequests := 0, 0, 0
			serviceAudience := "registry"
			if scenario == "quoted-opaque-service-audience" {
				serviceAudience = quotedServiceAudience
			}
			transport := hbnImageTransportFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.Host == "cdn.example" {
					foreignRequests++
					if scenario == "anonymous-config-redirect" {
						for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
							if request.Header.Get(header) != "" {
								t.Fatalf("credential/header %s escaped to blob CDN", header)
							}
						}
						if request.URL.Scheme != "https" || request.URL.Path != "/immutable/config" {
							t.Fatal("unsafe blob redirect")
						}
					}
					data := config
					if scenario == "blob-path-token-redirect" {
						data = []byte(`{"token":"must-not-be-fetched"}`)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(string(data))), Request: request}, nil
				}
				if request.URL.Scheme != "https" || request.URL.Host != "registry.example" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
					t.Fatal("metadata escaped explicit anonymous origin")
				}
				header := http.Header{}
				code := http.StatusOK
				data := root
				switch {
				case request.URL.Path == "/token" || scenario == "blob-path-token-redirect" && strings.Contains(request.URL.Path, "/blobs/sha256:"):
					tokenRequests++
					if request.URL.Host != "registry.example" || request.URL.Query().Get("scope") != "repository:qualified/hbn:pull" || request.Header.Get("Authorization") != "" {
						t.Fatal("token scope/credentials broadened")
					}
					if (scenario == "opaque-service-audience" || scenario == "quoted-opaque-service-audience") && request.URL.Query().Get("service") != serviceAudience {
						t.Fatal("opaque service audience was not passed to the same-origin token request")
					}
					data = []byte(`{"token":"anonymous-token"}`)
					if scenario == "blob-path-token-redirect" {
						code = http.StatusTemporaryRedirect
						header.Set("Location", "https://cdn.example/redirected-token")
					}
				case scenario == "malformed-challenge":
					code = http.StatusUnauthorized
					header.Set("WWW-Authenticate", `Basic realm="https://registry.example/token"`)
				case scenario == "foreign-realm":
					code = http.StatusUnauthorized
					header.Set("WWW-Authenticate", `Bearer realm="https://foreign.example/token",service="registry.example"`)
				case scenario == "foreign-redirect":
					code = http.StatusTemporaryRedirect
					header.Set("Location", "https://foreign.example/blob")
				case (scenario == "same-origin-anonymous-bearer" || scenario == "opaque-service-audience" || scenario == "quoted-opaque-service-audience" || scenario == "empty-service-audience" || scenario == "whitespace-service-audience" || scenario == "nonascii-service-audience" || scenario == "oversized-service-audience" || scenario == "blob-path-token-redirect" || scenario == "anonymous-config-redirect") && request.Header.Get("Authorization") == "":
					code = http.StatusUnauthorized
					service := "registry.example"
					switch scenario {
					case "opaque-service-audience":
						service = serviceAudience
					case "quoted-opaque-service-audience":
						service = quotedServiceAudience
					case "empty-service-audience":
						service = ""
					case "whitespace-service-audience":
						service = "registry audience"
					case "nonascii-service-audience":
						service = "réseau"
					case "oversized-service-audience":
						service = strings.Repeat("r", hbnTokenServiceLimit+1)
					}
					realm := "https://registry.example/token"
					if scenario == "blob-path-token-redirect" {
						realm = "https://registry.example/v2/qualified/hbn/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					}
					header.Set("WWW-Authenticate", `Bearer realm=`+strconv.Quote(realm)+`,service=`+strconv.Quote(service)+`,scope="repository:foreign:pull"`)
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
			success := scenario == "platform-manifest" || scenario == "image-index" || scenario == "same-origin-anonymous-bearer" || scenario == "opaque-service-audience" || scenario == "quoted-opaque-service-audience" || scenario == "anonymous-config-redirect"
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
			if scenario == "blob-path-token-redirect" && (requests != 2 || tokenRequests != 1 || foreignRequests != 0) {
				t.Fatalf("token redirect escaped same-origin boundary: requests=%d tokenRequests=%d foreignRequests=%d", requests, tokenRequests, foreignRequests)
			}
			if (scenario == "empty-service-audience" || scenario == "whitespace-service-audience" || scenario == "nonascii-service-audience" || scenario == "oversized-service-audience") && (requests != 1 || tokenRequests != 0) {
				t.Fatal("invalid service audience reached token endpoint")
			}
			if scenario == "malformed-challenge" && (requests != 1 || tokenRequests != 0) {
				t.Fatal("unsupported challenge reached token endpoint")
			}
		})
	}
}
