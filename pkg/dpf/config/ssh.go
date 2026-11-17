// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHAccess holds a snapshot of explicit authentication and host trust. It is
// runtime-only: saved configuration retains references, never their contents.
type SSHAccess struct {
	username, password     string
	privateKey, knownHosts []byte
	Sudo                   bool
}

func (a *SSHAccess) Credentials() (string, string) {
	if a == nil {
		return "", ""
	}
	return a.username, a.password
}
func (a *SSHAccess) PrivateKey() []byte {
	if a == nil {
		return nil
	}
	return bytes.Clone(a.privateKey)
}
func (a *SSHAccess) KnownHosts() []byte {
	if a == nil {
		return nil
	}
	return bytes.Clone(a.knownHosts)
}
func (a *SSHAccess) String() string   { return "<private SSH access>" }
func (a *SSHAccess) GoString() string { return a.String() }

// SSHOrigin accepts a host or host:port, with brackets for IPv6. It deliberately
// accepts no userinfo, path, SSH configuration alias, or proxy command.
func SSHOrigin(address string) (string, error) {
	if address == "" || strings.HasSuffix(address, ":") || strings.ContainsAny(address, " \t\r\n/@?#\\") || strings.Contains(address, "://") {
		return "", fmt.Errorf("expected SSH host or host:port")
	}
	u, err := url.Parse("ssh://" + address)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" {
		return "", fmt.Errorf("invalid SSH address")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", fmt.Errorf("IPv6 addresses require brackets")
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid SSH port")
	}
	return "ssh://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}

func resolveSSHAccess(c Credential, endpoint SSH, base string) (*SSHAccess, error) {
	if c.Username == "" || strings.ContainsAny(c.Username, "\x00\r\n") {
		return nil, fmt.Errorf("SSH credential requires a username")
	}
	a := &SSHAccess{username: c.Username, Sudo: endpoint.Sudo != nil && *endpoint.Sudo}
	if c.PrivateKeyRef != nil {
		if c.PasswordRef.Env != nil || c.PasswordRef.File != nil || c.PrivateKeyRef.File == "" {
			return nil, fmt.Errorf("SSH requires exactly one passwordRef or privateKeyRef")
		}
		key, err := regularFile(resolvePath(base, c.PrivateKeyRef.File), 1<<20)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve SSH private key")
		}
		if _, err := ssh.ParsePrivateKey(key); err != nil {
			return nil, fmt.Errorf("invalid or encrypted SSH private key")
		}
		a.privateKey = key
	} else {
		password, err := resolvePassword(c.PasswordRef, base)
		if err != nil {
			return nil, err
		}
		a.password = password
	}
	if endpoint.KnownHostsFile == "" {
		return nil, fmt.Errorf("knownHostsFile is required")
	}
	known, err := regularFile(resolvePath(base, endpoint.KnownHostsFile), 1<<20)
	if err != nil || len(bytes.TrimSpace(known)) == 0 {
		return nil, fmt.Errorf("cannot resolve nonempty SSH knownHostsFile")
	}
	a.knownHosts = known
	return a, nil
}
