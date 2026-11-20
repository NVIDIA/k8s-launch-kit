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
	if err := validateSSHAccess(c, endpoint); err != nil {
		return nil, err
	}
	a, err := resolveSSHAuthentication(c, base)
	if err != nil {
		return nil, err
	}
	a.knownHosts, err = resolveSSHTrust(endpoint, base)
	if err != nil {
		return nil, err
	}
	a.Sudo = endpoint.Sudo != nil && *endpoint.Sudo
	return a, nil
}

func resolveSSHAuthentication(c Credential, base string) (*SSHAccess, error) {
	a := &SSHAccess{username: c.Username}
	if c.PrivateKeyRef != nil {
		key, err := regularFile(resolvePath(base, c.PrivateKeyRef.File), 1<<20)
		if err != nil {
			return nil, &accessMaterialError{message: "cannot resolve SSH private key"}
		}
		if _, err := ssh.ParsePrivateKey(key); err != nil {
			return nil, &accessMaterialError{message: "invalid or encrypted SSH private key"}
		}
		a.privateKey = key
	} else {
		password, err := resolvePassword(c.PasswordRef, base)
		if err != nil {
			return nil, err
		}
		a.password = password
	}
	return a, nil
}

func resolveSSHTrust(endpoint SSH, base string) ([]byte, error) {
	known, err := regularFile(resolvePath(base, endpoint.KnownHostsFile), 1<<20)
	if err != nil || len(bytes.TrimSpace(known)) == 0 {
		return nil, &accessMaterialError{message: "cannot resolve nonempty SSH knownHostsFile"}
	}
	return known, nil
}

func validateSSHAccess(c Credential, endpoint SSH) error {
	if c.Username == "" || strings.ContainsAny(c.Username, "\x00\r\n") {
		return fmt.Errorf("SSH credential requires a username")
	}
	if c.PrivateKeyRef != nil {
		if c.PasswordRef.Env != nil || c.PasswordRef.File != nil || c.PrivateKeyRef.File == "" {
			return fmt.Errorf("SSH requires exactly one passwordRef or privateKeyRef")
		}
	} else {
		if err := validatePasswordRef(c.PasswordRef); err != nil {
			return err
		}
	}
	if endpoint.KnownHostsFile == "" {
		return fmt.Errorf("knownHostsFile is required")
	}
	return nil
}
