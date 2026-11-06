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
	Tool  string
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

// preferredHostKeyAlgorithms avoids negotiating an unpinned server key when a
// different advertised key is already trusted. Host matching and revocation
// decisions belong to the upstream callback, including hashed host entries.
// Certificates remain available and are verified by that same callback: the
// signing CA's algorithm does not determine the host certificate's key type.
func preferredHostKeyAlgorithms(ctx context.Context, data []byte, verify ssh.HostKeyCallback, address string, remote net.Addr) ([]string, error) {
	preferred := map[string]bool{}
	seen := map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if ctx.Err() != nil {
			return nil, &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: "known-host-algorithms"}
		}
		fields := bytes.Fields(line)
		if len(fields) < 3 || fields[0][0] == '#' || fields[0][0] == '@' {
			continue
		}
		// New already validated the file. Parse just its key fields so arbitrary
		// multi-word comments do not change ParseKnownHosts field-count handling.
		_, _, key, _, _, err := ssh.ParseKnownHosts(bytes.Join(fields[:3], []byte{' '}))
		if err != nil {
			return nil, &Error{Code: "SSH_TRUST_INVALID", Stage: "known-host-algorithms"}
		}
		blob := string(key.Marshal())
		if seen[blob] {
			continue
		}
		seen[blob] = true
		if verify(address, remote, key) != nil {
			continue
		}
		switch key.Type() {
		case ssh.KeyAlgoRSA:
			preferred[ssh.KeyAlgoRSASHA256], preferred[ssh.KeyAlgoRSASHA512] = true, true
		default:
			preferred[key.Type()] = true
		}
	}
	supported := ssh.SupportedAlgorithms().HostKeys
	result := make([]string, 0, len(supported))
	for _, a := range supported {
		if preferred[a] {
			result = append(result, a)
		}
	}
	for _, a := range supported {
		if !preferred[a] {
			result = append(result, a)
		}
	}
	return result, nil
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
	algorithms, err := preferredHostKeyAlgorithms(dialCtx, auth.knownHosts, verify, u.Host, conn.RemoteAddr())
	if err != nil {
		return nil, err
	}
	trustFailed := false
	cfg := &ssh.ClientConfig{User: auth.user, Auth: []ssh.AuthMethod{method}, HostKeyAlgorithms: algorithms, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
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
		code := "SSH_COMMAND_FAILED"
		if ctx.Err() != nil {
			code = "SSH_TIMEOUT_OR_CANCELLED"
		}
		return "", &Error{Code: code, Stage: stage, Tool: commandTool(command)}
	}
	defer session.Close()
	output := &commandOutput{}
	session.Stdout = outputWriter{output: output}
	session.Stderr = outputWriter{output: output, stderr: true}
	err = session.Run("LC_ALL=C " + command)
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.exceeded {
		return "", &Error{Code: "SSH_OUTPUT_LIMIT", Stage: stage, Tool: commandTool(command)}
	}
	if ctx.Err() != nil {
		return "", &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: stage, Tool: commandTool(command)}
	}
	if err != nil {
		return "", commandFailure(err, stage, command, output.stdout.String()+"\n"+output.stderr.String())
	}
	return output.stdout.String(), nil
}
