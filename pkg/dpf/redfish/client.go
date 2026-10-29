// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package redfish implements a bounded GET/session-only Redfish transport.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

type Document map[string]any

// Error deliberately omits response bodies, headers and private credentials.
type Error struct {
	Code   string
	URI    string
	Status int
}

func (e *Error) Error() string {
	return fmt.Sprintf("Redfish %s at %s (HTTP %d)", e.Code, e.URI, e.Status)
}
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "BMC_TIMEOUT"
	}
	return "BMC_UNAVAILABLE"
}

// Reader intentionally has no POST/PATCH/action method available to adapters.
type Reader interface {
	Get(context.Context, string) (Document, error)
	Collection(context.Context, string) ([]Resource, error)
}
type Resource struct {
	URI      string
	Document Document
}
type Client struct {
	origin           string
	http             *http.Client
	access           config.Access
	token            string
	session          string
	sessionUncertain bool
	reads            int
	bytes            int
}

func New(target config.Target, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = target.Access.TLSConfig()
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return &Client{origin: target.Endpoint, access: target.Access, http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) guard(ref string) (string, error) {
	u, e := url.Parse(ref)
	if e != nil {
		return "", &Error{Code: "UNSAFE_LINK"}
	}
	if u.Host != "" && !u.IsAbs() {
		return "", &Error{Code: "CROSS_ORIGIN_LINK"}
	}
	if u.IsAbs() {
		o, err := config.Origin(u.Scheme + "://" + u.Host)
		if err != nil || o != c.origin {
			return "", &Error{Code: "CROSS_ORIGIN_LINK"}
		}
	}
	if u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/redfish/v1/") || strings.Contains(u.Path, "..") || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") {
		return "", &Error{Code: "UNSAFE_LINK"}
	}
	// Queries are accepted only as explicit pagination $skip/$skiptoken parameters.
	for key := range u.Query() {
		if key != "$skip" && key != "$skiptoken" {
			return "", &Error{Code: "UNSAFE_QUERY"}
		}
	}
	u.Scheme = ""
	u.Host = ""
	return u.String(), nil
}
func (c *Client) request(ctx context.Context, method, ref string, body []byte) (Document, http.Header, error) {
	uri, e := c.guard(ref)
	if e != nil {
		return nil, nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, c.origin+uri, bytes.NewReader(body))
	if e != nil {
		return nil, nil, &Error{Code: "REQUEST_INVALID", URI: uri}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Auth-Token", c.token)
	}
	response, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		var verification *tls.CertificateVerificationError
		if errors.As(e, &verification) {
			return nil, nil, &Error{Code: "TLS_VERIFICATION_FAILED", URI: uri}
		}
		return nil, nil, &Error{Code: "TRANSPORT_FAILED", URI: uri}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "HTTP_FAILED"
		if response.StatusCode == 401 {
			code = "AUTHENTICATION_FAILED"
		}
		if response.StatusCode == 403 {
			code = "ACCESS_DENIED"
		}
		return nil, nil, &Error{Code: code, URI: uri, Status: response.StatusCode}
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	c.bytes += len(b)
	if e != nil || len(b) > 4<<20 || c.bytes > 32<<20 {
		return nil, nil, &Error{Code: "RESPONSE_LIMIT", URI: uri}
	}
	var d Document
	if len(b) != 0 {
		if e := json.Unmarshal(b, &d); e != nil || d == nil {
			return nil, nil, &Error{Code: "INVALID_RESPONSE", URI: uri}
		}
	}
	return d, response.Header.Clone(), nil
}
func (c *Client) Get(ctx context.Context, ref string) (Document, error) {
	c.reads++
	if c.reads > 256 {
		return nil, &Error{Code: "RESOURCE_LIMIT"}
	}
	for attempt := 0; attempt < 3; attempt++ {
		d, _, err := c.request(ctx, http.MethodGet, ref, nil)
		if err == nil {
			return d, nil
		}
		var remote *Error
		retry := errors.As(err, &remote) && (remote.Code == "TRANSPORT_FAILED" || remote.Status == 429 || remote.Status == 502 || remote.Status == 503 || remote.Status == 504)
		if !retry || attempt == 2 {
			return nil, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, &Error{Code: "BMC_UNAVAILABLE"}
}
func (c *Client) Open(ctx context.Context) (Document, error) {
	root, err := c.Get(ctx, "/redfish/v1/")
	if err != nil {
		return nil, err
	}
	service := Link(root, "SessionService")
	if service == "" {
		return nil, &Error{Code: "SESSION_AUTH_UNSUPPORTED"}
	}
	service, err = c.guard(service)
	if err != nil {
		return nil, err
	}
	sd, err := c.Get(ctx, service)
	if err != nil {
		return nil, err
	}
	collection := Link(sd, "Sessions")
	if collection == "" {
		return nil, &Error{Code: "SESSION_AUTH_UNSUPPORTED"}
	}
	collection, err = c.guard(collection)
	if err != nil {
		return nil, err
	}
	if service != "/redfish/v1/SessionService" || collection != "/redfish/v1/SessionService/Sessions" {
		return nil, &Error{Code: "SESSION_URI_UNSUPPORTED"}
	}
	username, password := c.access.Credentials()
	body, _ := json.Marshal(map[string]string{"UserName": username, "Password": password})
	// Never retry an ambiguous login. The token and session URI are never serialized.
	c.sessionUncertain = true
	_, header, err := c.request(ctx, http.MethodPost, collection, body)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 && failure.Status != 408 {
			c.sessionUncertain = false
		}
	}
	clear(body)
	if err != nil {
		return nil, err
	}
	token := header.Get("X-Auth-Token")
	session, err := c.guard(header.Get("Location"))
	if err != nil || token == "" || !strings.HasPrefix(session, collection+"/") || strings.Contains(strings.TrimPrefix(session, collection+"/"), "/") || strings.Contains(session, "?") {
		return nil, &Error{Code: "INVALID_SESSION_RECEIPT"}
	}
	c.token = token
	c.session = session
	c.sessionUncertain = false
	return root, nil
}
func (c *Client) Close(ctx context.Context) error {
	defer func() { c.token = ""; c.http.CloseIdleConnections() }()
	if c.session != "" {
		_, _, err := c.request(ctx, http.MethodDelete, c.session, nil)
		return err
	}
	if c.sessionUncertain {
		return &Error{Code: "SESSION_OUTCOME_UNKNOWN"}
	}
	return nil
}
func (c *Client) Collection(ctx context.Context, ref string) ([]Resource, error) {
	var members []Resource
	seen := map[string]bool{}
	count := -1
	initial, err := c.guard(ref)
	if err != nil {
		return nil, err
	}
	base := strings.Split(initial, "?")[0]
	for page := 0; ref != ""; page++ {
		if page >= 32 || seen[ref] {
			return nil, &Error{Code: "PAGINATION_LIMIT"}
		}
		seen[ref] = true
		normalized, err := c.guard(ref)
		if err != nil || strings.Split(normalized, "?")[0] != base {
			return nil, &Error{Code: "UNSAFE_PAGINATION"}
		}
		d, err := c.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		if n, ok := d["Members@odata.count"].(float64); ok {
			if n < 0 || n != float64(int(n)) || count >= 0 && count != int(n) {
				return nil, &Error{Code: "COLLECTION_CHANGED"}
			}
			count = int(n)
		}
		list, ok := d["Members"].([]any)
		if !ok {
			return nil, &Error{Code: "INCOMPLETE_COLLECTION"}
		}
		for _, item := range list {
			link := Link(Document{"member": item}, "member")
			link, err = c.guard(link)
			if err != nil {
				return nil, err
			}
			if link == "" || seen["member:"+link] {
				return nil, &Error{Code: "DUPLICATE_MEMBER"}
			}
			seen["member:"+link] = true
			value, err := c.Get(ctx, link)
			if err != nil {
				return nil, err
			}
			members = append(members, Resource{URI: link, Document: value})
		}
		ref = Text(d, "Members@odata.nextLink")
	}
	if count < 0 || count != len(members) {
		return nil, &Error{Code: "INCOMPLETE_COLLECTION"}
	}
	return members, nil
}
func Text(d Document, k string) string { s, _ := d[k].(string); return s }
func Object(d Document, k string) Document {
	switch v := d[k].(type) {
	case Document:
		return v
	case map[string]any:
		return Document(v)
	default:
		return nil
	}
}
func Link(d Document, k string) string { return Text(Object(d, k), "@odata.id") }
