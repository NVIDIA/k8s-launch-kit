// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	strictjson "sigs.k8s.io/json"
	"strings"
	"time"
)

const hbnImageMetadataLimit = 2 << 20
const hbnManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
const hbnDockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"

type hbnImageDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

// resolveHBNRuntimeImage reads only bounded anonymous immutable OCI metadata.
// It never uses Docker/Helm credential stores, ambient proxies or a credentialed
// foreign-origin authenticated redirect. Registry credentials are not a new user input.
func resolveHBNRuntimeImage(ctx context.Context, reference string, timeout time.Duration, transport http.RoundTripper) (hbnPodImageIdentity, error) {
	bad := func() (hbnPodImageIdentity, error) {
		return hbnPodImageIdentity{}, fmt.Errorf("HBN_RUNTIME_IMAGE_IDENTITY_UNAVAILABLE")
	}
	if ctx == nil || timeout < time.Second || timeout > 2*time.Minute {
		return bad()
	}
	if _, ok := ctx.Deadline(); !ok {
		return bad()
	}
	repository := hbnImageRepository(reference)
	host, path, ok := strings.Cut(repository, "/")
	_, digest, pinned := strings.Cut(reference, "@")
	if !ok || !pinned || !validSHA256Digest(digest) || host == "" || path == "" || strings.ContainsAny(host, "@?#") || strings.ContainsAny(path, "?#") {
		return bad()
	}
	origin, err := url.Parse("https://" + host)
	if err != nil || origin.Host != host || origin.Path != "" || origin.User != nil {
		return bad()
	}
	if transport == nil {
		base := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: min(timeout, 10*time.Second), ResponseHeaderTimeout: timeout}
		defer base.CloseIdleConnections()
		transport = base
	}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		// Only anonymous blob transfers may cross origins. Tokens and manifests
		// must stay on their explicit source; every blob is verified by its locked
		// config descriptor after the last response.
		if len(via) == 0 || len(via) > 3 || !strings.HasPrefix(via[0].URL.Path, "/v2/"+path+"/blobs/sha256:") {
			return http.ErrUseLastResponse
		}
		if request.URL.Scheme != "https" || request.URL.Host == "" || request.URL.User != nil || request.URL.Fragment != "" {
			return http.ErrUseLastResponse
		}
		for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
			request.Header.Del(header)
		}
		return nil
	}}
	token := ""
	fetch := func(kind, digest string) ([]byte, error) {
		endpoint := *origin
		endpoint.Path = "/v2/" + path + "/" + kind + "/" + digest
		for attempt := 0; attempt < 2; attempt++ {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
			if err != nil {
				return nil, err
			}
			request.Header.Set("Accept-Encoding", "identity")
			request.Header.Set("Accept", hbnManifestMediaType+", "+hbnDockerManifestMediaType+", application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response, err := client.Do(request)
			if err != nil {
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				return nil, fmt.Errorf("registry unavailable")
			}
			if response.StatusCode == http.StatusUnauthorized && attempt == 0 && token == "" {
				challenge := response.Header.Get("WWW-Authenticate")
				_ = response.Body.Close()
				if !strings.HasPrefix(challenge, "Bearer ") {
					return nil, fmt.Errorf("unsupported registry authorization")
				}
				_, params, err := mime.ParseMediaType("application/bearer;" + strings.ReplaceAll(strings.TrimPrefix(challenge, "Bearer "), ",", ";"))
				if err != nil {
					return nil, err
				}
				realm, err := url.Parse(params["realm"])
				if err != nil || realm.Scheme != "https" || realm.Host != origin.Host || realm.User != nil || realm.RawQuery != "" || realm.Fragment != "" || realm.Path == "" {
					return nil, fmt.Errorf("foreign registry authorization")
				}
				query := url.Values{"scope": {"repository:" + path + ":pull"}}
				if service := params["service"]; service != "" {
					if service != host {
						return nil, fmt.Errorf("foreign token service")
					}
					query.Set("service", service)
				}
				realm.RawQuery = query.Encode()
				authRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
				if err != nil {
					return nil, err
				}
				authResponse, err := client.Do(authRequest)
				if err != nil {
					if authResponse != nil && authResponse.Body != nil {
						_ = authResponse.Body.Close()
					}
					return nil, err
				}
				data, err := readHBNImageResponse(authResponse)
				if err != nil {
					return nil, err
				}
				var auth struct {
					Token       string `json:"token"`
					AccessToken string `json:"access_token"`
				}
				if json.Unmarshal(data, &auth) != nil {
					return nil, fmt.Errorf("invalid registry authorization")
				}
				token = auth.Token
				if token == "" {
					token = auth.AccessToken
				}
				if token == "" || len(token) > 16<<10 || strings.ContainsAny(token, " \t\r\n\x00") {
					return nil, fmt.Errorf("invalid registry token")
				}
				continue
			}
			data, err := readHBNImageResponse(response)
			if err != nil {
				return nil, err
			}
			if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
				return nil, fmt.Errorf("immutable metadata digest mismatch")
			}
			var document any
			strictErrors, decodeErr := strictjson.UnmarshalStrict(data, &document, strictjson.DisallowDuplicateFields)
			if decodeErr != nil || len(strictErrors) != 0 {
				return nil, fmt.Errorf("ambiguous image metadata")
			}
			return data, nil
		}
		return nil, fmt.Errorf("registry authorization unavailable")
	}
	data, err := fetch("manifests", digest)
	if err != nil {
		return bad()
	}
	var manifest struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Config        hbnImageDescriptor   `json:"config"`
		Manifests     []hbnImageDescriptor `json:"manifests"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 {
		return bad()
	}
	if manifest.MediaType == "application/vnd.oci.image.index.v1+json" || manifest.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var selected *hbnImageDescriptor
		for i := range manifest.Manifests {
			descriptor := &manifest.Manifests[i]
			if descriptor.Platform.OS != "linux" || descriptor.Platform.Architecture != "arm64" || descriptor.Platform.Variant != "" && descriptor.Platform.Variant != "v8" {
				continue
			}
			if selected != nil {
				return bad()
			}
			selected = descriptor
		}
		if selected == nil || !validHBNImageDescriptor(*selected) || selected.MediaType != hbnManifestMediaType && selected.MediaType != hbnDockerManifestMediaType {
			return bad()
		}
		digest = selected.Digest
		data, err = fetch("manifests", digest)
		if err != nil || int64(len(data)) != selected.Size {
			return bad()
		}
		manifest.Manifests = nil
		if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 {
			return bad()
		}
	}
	if manifest.MediaType != hbnManifestMediaType && manifest.MediaType != hbnDockerManifestMediaType || !validHBNImageDescriptor(manifest.Config) ||
		manifest.Config.MediaType != "application/vnd.oci.image.config.v1+json" && manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return bad()
	}
	config, err := fetch("blobs", manifest.Config.Digest)
	if err != nil || int64(len(config)) != manifest.Config.Size {
		return bad()
	}
	var platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	if json.Unmarshal(config, &platform) != nil || platform.OS != "linux" || platform.Architecture != "arm64" {
		return bad()
	}
	return hbnPodImageIdentity{Reference: reference, ManifestDigest: digest, ConfigDigest: manifest.Config.Digest}, nil
}
func validHBNImageDescriptor(descriptor hbnImageDescriptor) bool {
	return validSHA256Digest(descriptor.Digest) && descriptor.Size > 0 && descriptor.Size <= hbnImageMetadataLimit
}
func readHBNImageResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("missing metadata response")
	}
	defer response.Body.Close()
	encoding := response.Header.Get("Content-Encoding")
	if response.StatusCode != http.StatusOK || response.ContentLength > hbnImageMetadataLimit || encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("metadata response refused")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, hbnImageMetadataLimit+1))
	if err != nil || len(data) == 0 || len(data) > hbnImageMetadataLimit {
		return nil, fmt.Errorf("metadata response incomplete")
	}
	return data, nil
}
