// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package sshprobe observes explicitly selected Linux hosts and DPU ARM endpoints.
// It never installs software, changes configuration, or connects to an address
// learned from the remote system.
package sshprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const maxOutput = 1 << 20

// Error carries a stable diagnostic without remote output or credential material.
type Error struct {
	Code  string
	Stage string
}

func (e *Error) Error() string { return e.Code + ": " + e.Stage }

// Code classifies errors without exposing SSH library or remote error text.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "SSH_OBSERVATION_FAILED"
}

type credentials struct {
	user, password  string
	key, knownHosts []byte
}

type client struct {
	ssh     *ssh.Client
	stop    func() bool
	timeout time.Duration
}

// hostKeyCallback loads a stable snapshot through the upstream known_hosts
// parser, preserving its hashed-host, wildcard, CA and revocation semantics.
// The temporary file contains public host keys only and is removed before dial.
func hostKeyCallback(data []byte) (ssh.HostKeyCallback, error) {
	if len(bytes.TrimSpace(data)) == 0 || len(data) > maxOutput {
		return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-hosts"}
	}
	f, err := os.CreateTemp("", "l8k-known-hosts-*")
	if err != nil {
		return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-hosts"}
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-hosts"}
	}
	if err = f.Close(); err != nil {
		return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-hosts"}
	}
	callback, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-hosts"}
	}
	return callback, nil
}

func connect(ctx context.Context, endpoint string, auth credentials, timeout time.Duration) (*client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "ssh" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || u.Port() == "" || timeout <= 0 {
		return nil, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "endpoint"}
	}
	verify, err := hostKeyCallback(auth.knownHosts)
	if err != nil {
		return nil, err
	}
	if auth.user == "" || (auth.password == "") == (len(auth.key) == 0) {
		return nil, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "credential"}
	}
	var method ssh.AuthMethod
	if len(auth.key) > 0 {
		signer, parseErr := ssh.ParsePrivateKey(auth.key)
		if parseErr != nil {
			return nil, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "private-key"}
		}
		method = ssh.PublicKeys(signer)
	} else {
		method = ssh.Password(auth.password)
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", u.Host)
	if err != nil {
		return nil, &Error{Code: "SSH_CONNECT_FAILED", Stage: "dial"}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	failed := true
	defer func() {
		if failed {
			stop()
			_ = conn.Close()
		}
	}()
	deadline, _ := dialCtx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, &Error{Code: "SSH_CONNECT_FAILED", Stage: "deadline"}
	}
	trustFailed := false
	cfg := &ssh.ClientConfig{User: auth.user, Auth: []ssh.AuthMethod{method}, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(host, remote, key)
		trustFailed = err != nil
		return err
	}}
	cc, channels, requests, err := ssh.NewClientConn(conn, u.Host, cfg)
	if err != nil {
		code := "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED"
		if trustFailed {
			code = "SSH_HOST_KEY_REJECTED"
		}
		return nil, &Error{Code: code, Stage: "handshake"}
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		_ = cc.Close()
		return nil, &Error{Code: "SSH_CONNECT_FAILED", Stage: "deadline"}
	}
	failed = false
	return &client{ssh: ssh.NewClient(cc, channels, requests), stop: stop, timeout: timeout}, nil
}

func (c *client) Close() {
	c.stop()
	_ = c.ssh.Close()
}

type commandOutput struct {
	mu             sync.Mutex
	stdout, stderr bytes.Buffer
	size           int
	exceeded       bool
}
type outputWriter struct {
	output *commandOutput
	stderr bool
}

func (w outputWriter) Write(p []byte) (int, error) {
	o := w.output
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(p) > maxOutput-o.size {
		o.exceeded = true
		return 0, fmt.Errorf("SSH output limit exceeded")
	}
	o.size += len(p)
	if w.stderr {
		return o.stderr.Write(p)
	}
	return o.stdout.Write(p)
}

func (c *client) Run(ctx context.Context, stage, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// Closing the connection also interrupts NewSession/Start if the server stops
	// responding. A timed-out connection is not reused for later observations.
	stop := context.AfterFunc(ctx, func() { _ = c.ssh.Close() })
	defer stop()
	session, err := c.ssh.NewSession()
	if err != nil {
		return "", &Error{Code: "SSH_COMMAND_FAILED", Stage: stage}
	}
	defer session.Close()
	output := &commandOutput{}
	session.Stdout = outputWriter{output: output}
	session.Stderr = outputWriter{output: output, stderr: true}
	err = session.Run("LC_ALL=C " + command)
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.exceeded {
		return "", &Error{Code: "SSH_OUTPUT_LIMIT", Stage: stage}
	}
	if ctx.Err() != nil {
		return "", &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: stage}
	}
	if err != nil {
		return "", &Error{Code: "SSH_COMMAND_FAILED", Stage: stage}
	}
	return output.stdout.String(), nil
}
