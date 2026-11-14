// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package probe contains the public diagnostic protocol and Kubernetes runner.
package probe

import (
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const ProtocolVersion = "v1alpha1"

// collectorScript adapts the existing image's tools to our public result format.
// It is fixed release code, never site-supplied commands or a downloaded helper.
//
//go:embed collect.sh
var collectorScript string

type Destination struct {
	ID         string `json:"id"`
	Endpoint   string `json:"endpoint"`
	Path       string `json:"path"`
	ServerName string `json:"serverName,omitempty"`
	CA         []byte `json:"ca,omitempty"`
}
type Request struct {
	RequestTimeout time.Duration `json:"requestTimeout"`
	Version        string        `json:"version"`
	RunID          string        `json:"runID"`
	Deadline       time.Time     `json:"deadline"`
	Destinations   []Destination `json:"destinations"`
}
type Response struct {
	StartedAt   time.Time               `json:"startedAt"`
	FinishedAt  time.Time               `json:"finishedAt"`
	Version     string                  `json:"version"`
	RunID       string                  `json:"runID"`
	NodeUID     string                  `json:"nodeUID"`
	PodUID      string                  `json:"podUID"`
	NetworkMode string                  `json:"networkMode"`
	Results     []inventory.ProbeResult `json:"results"`
	Interfaces  []inventory.Interface   `json:"interfaces,omitempty"`
	Routes      []inventory.Route       `json:"routes,omitempty"`
}

type toolDestination struct {
	ID         string `json:"id"`
	Hostname   string `json:"hostname"`
	Port       string `json:"port"`
	Literal    bool   `json:"literal"`
	URL        string `json:"url"`
	Authority  string `json:"authority"`
	VerifyName string `json:"verifyName"`
	CAFile     string `json:"caFile"`
}

// requestFiles resolves URL/trust coordinates in Go. The script consumes JSON
// values as quoted arguments; it never parses URLs or evaluates site input.
func requestFiles(request Request) (map[string]string, error) {
	if request.Version != ProtocolVersion || request.RunID == "" || len(request.Destinations) == 0 || len(request.Destinations) > 257 || time.Until(request.Deadline) > time.Hour || time.Until(request.Deadline) <= 0 || request.RequestTimeout < time.Second || request.RequestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("invalid probe request scope/deadline")
	}
	files := map[string]string{"collect.sh": collectorScript}
	destinations := []toolDestination{}
	seen := map[string]bool{}
	for i, destination := range request.Destinations {
		u, err := url.Parse(destination.Endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || strings.ContainsAny(u.Host, "\r\n") || !strings.HasPrefix(destination.Path, "/") || destination.ID == "" || seen[destination.ID] {
			return nil, fmt.Errorf("invalid or duplicate probe destination")
		}
		seen[destination.ID] = true
		port := u.Port()
		if port == "" {
			port = "443"
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid destination port")
		}
		name := destination.ServerName
		if name == "" {
			name = u.Hostname()
		}
		if strings.ContainsAny(name, "\r\n /@?#%") {
			return nil, fmt.Errorf("invalid TLS server name")
		}
		host := name
		if strings.Contains(host, ":") {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, fmt.Errorf("invalid TLS server name")
			}
			host = "[" + host + "]"
		}
		probeURL := url.URL{Scheme: "https", Host: host + ":" + port, Path: destination.Path}
		_, literalErr := netip.ParseAddr(u.Hostname())
		d := toolDestination{ID: destination.ID, Hostname: u.Hostname(), Port: port, Literal: literalErr == nil, URL: probeURL.String(), Authority: u.Host, VerifyName: name}
		if len(destination.CA) > 0 {
			if !x509.NewCertPool().AppendCertsFromPEM(destination.CA) {
				return nil, fmt.Errorf("invalid public probe CA")
			}
			key := fmt.Sprintf("ca-%d.pem", i)
			files[key] = string(destination.CA)
			d.CAFile = "/request/" + key
		}
		destinations = append(destinations, d)
	}
	document := struct {
		Version        string            `json:"version"`
		RunID          string            `json:"runID"`
		Deadline       time.Time         `json:"deadline"`
		RequestTimeout time.Duration     `json:"requestTimeout"`
		Destinations   []toolDestination `json:"destinations"`
	}{request.Version, request.RunID, request.Deadline.UTC(), request.RequestTimeout, destinations}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	files["request.json"] = string(data)
	size := 0
	for key, value := range files {
		size += len(key) + len(value)
	}
	if size > 900<<10 {
		return nil, fmt.Errorf("probe request exceeds ConfigMap budget")
	}
	return files, nil
}
