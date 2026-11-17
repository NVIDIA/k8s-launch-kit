// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A local peer closes before server authentication is even reached. The same
// existing combined code therefore cannot establish rejected credentials.
func TestEarlySSHPeerClosureHasNoCredentialFailureProof(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "EOF"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			result := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err == nil && !strings.HasPrefix(line, "SSH-") {
					err = errors.New("client version not observed")
				}
				if reset {
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
				}
				result <- err
			}()
			_, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			key, err := ssh.NewSignerFromKey(private)
			if err != nil {
				t.Fatal(err)
			}
			auth := credentials{user: "offline", password: "synthetic", knownHosts: []byte(knownhosts.Line([]string{listener.Addr().String()}, key.PublicKey()) + "\n")}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := connect(ctx, "ssh://"+listener.Addr().String(), auth, time.Second)
			if client != nil {
				_ = client.Close()
				t.Fatal("early closed peer authenticated")
			}
			var typed *Error
			if !errors.As(err, &typed) || typed == nil || typed.Code != "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED" || typed.Stage != "handshake" {
				t.Fatalf("classification=%v", err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}
