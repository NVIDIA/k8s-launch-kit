// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type testServer struct {
	listener    net.Listener
	key         ssh.Signer
	private     ed25519.PrivateKey
	mu          sync.Mutex
	connections []net.Conn
	wg          sync.WaitGroup
	closing     bool
}

func startServer() *testServer {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	key, err := ssh.NewSignerFromKey(private)
	Expect(err).NotTo(HaveOccurred())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	s := &testServer{listener: listener, key: key, private: private, connections: []net.Conn{}}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closing {
				s.mu.Unlock()
				_ = c.Close()
				return
			}
			s.connections = append(s.connections, c)
			s.wg.Add(1)
			s.mu.Unlock()
			go func() { defer s.wg.Done(); defer c.Close(); s.serve(c) }()
		}
	}()
	return s
}

func (s *testServer) serve(conn net.Conn) {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "reader" && string(key.Marshal()) == string(s.key.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}, PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() == "reader" && string(password) == "test-password" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(s.key)
	server, chans, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	var sessions sync.WaitGroup
	defer sessions.Wait()
	for channel := range chans {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, reqs, err := channel.Accept()
		if err != nil {
			return
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer ch.Close()
			for req := range reqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if ssh.Unmarshal(req.Payload, &payload) != nil {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				switch payload.Command {
				case "LC_ALL=C slow":
					for range reqs {
					}
					return
				case "LC_ALL=C flood":
					_, _ = ch.Write([]byte(strings.Repeat("x", maxOutput+1)))
				case "LC_ALL=C fail":
					_, _ = ch.Stderr().Write([]byte("remote sensitive diagnostic"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{7}))
					return
				default:
					_, _ = ch.Write([]byte("observed\n"))
					_, _ = ch.Stderr().Write([]byte("warning\n"))
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

func (s *testServer) close() {
	s.mu.Lock()
	s.closing = true
	_ = s.listener.Close()
	for _, c := range s.connections {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *testServer) auth() credentials {
	return credentials{user: "reader", password: "test-password", knownHosts: []byte(knownhosts.Line([]string{s.listener.Addr().String()}, s.key.PublicKey()) + "\n")}
}

var _ = Describe("bounded SSH transport", func() {
	var server *testServer
	BeforeEach(func() { server = startServer() })
	AfterEach(func() { server.close() })
	It("verifies host identity before authentication and keeps stdout separate", func() {
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()
		out, err := c.Run(context.Background(), "query", "query")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("observed\n"))
	})
	It("accepts hashed known-hosts entries through the upstream parser", func() {
		auth := server.auth()
		auth.knownHosts = []byte(knownhosts.HashHostname(knownhosts.Normalize(server.listener.Addr().String())) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(server.key.PublicKey()))) + "\n")
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).NotTo(HaveOccurred())
		c.Close()
	})
	It("uses only the explicitly provided private key", func() {
		block, err := ssh.MarshalPrivateKey(server.private, "test")
		Expect(err).NotTo(HaveOccurred())
		auth := server.auth()
		auth.password = ""
		auth.key = pem.EncodeToMemory(block)
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).NotTo(HaveOccurred())
		c.Close()
		auth.key = []byte("invalid-private-key")
		_, err = connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(Code(err)).To(Equal("SSH_CONFIGURATION_INVALID"))
		Expect(err.Error()).NotTo(ContainSubstring("invalid-private-key"))
	})
	It("rejects unknown and changed host keys", func() {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		other, err := ssh.NewSignerFromKey(private)
		Expect(err).NotTo(HaveOccurred())
		for _, entry := range []string{knownhosts.Line([]string{"other.example"}, server.key.PublicKey()), knownhosts.Line([]string{server.listener.Addr().String()}, other.PublicKey())} {
			auth := server.auth()
			auth.knownHosts = []byte(entry + "\n")
			_, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
			Expect(Code(err)).To(Equal("SSH_HOST_KEY_REJECTED"))
		}
	})
	It("does not expose rejected credentials or remote stderr", func() {
		auth := server.auth()
		auth.password = "do-not-print"
		_, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring(auth.password))
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()
		_, err = c.Run(context.Background(), "failure", "fail")
		Expect(Code(err)).To(Equal("SSH_COMMAND_FAILED"))
		Expect(err.Error()).NotTo(ContainSubstring("sensitive"))
	})
	It("bounds command output", func() {
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()
		_, err = c.Run(context.Background(), "large", "flood")
		Expect(Code(err)).To(Equal("SSH_OUTPUT_LIMIT"))
	})
	It("cancels a command and closes its transport", func() {
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = c.Run(ctx, "slow", "slow")
		Expect(Code(err)).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})
	It("bounds an unresponsive handshake", func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer listener.Close()
		done := make(chan struct{})
		go func() {
			conn, e := listener.Accept()
			if e == nil {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					if _, e = conn.Read(buf); e != nil {
						break
					}
				}
			}
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = connect(ctx, "ssh://"+listener.Addr().String(), server.auth(), time.Second)
		Expect(err).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
		Eventually(done).Should(BeClosed())
	})
})
