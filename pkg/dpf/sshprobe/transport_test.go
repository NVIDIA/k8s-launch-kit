// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type testServer struct {
	listener     net.Listener
	key          ssh.Signer
	private      ed25519.PrivateKey
	extraKeys    []ssh.Signer
	alternateKey ssh.Signer
	rekeyAfter   int
	rejectAfter  int
	onCommand    func(ssh.Channel, string) bool
	mu           sync.Mutex
	connections  []net.Conn
	wg           sync.WaitGroup
	closing      bool
}

func startServer(extraKeys ...ssh.Signer) *testServer {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	key, err := ssh.NewSignerFromKey(private)
	Expect(err).NotTo(HaveOccurred())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	s := &testServer{listener: listener, key: key, private: private, extraKeys: extraKeys, connections: []net.Conn{}}
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
			connection := len(s.connections)
			s.wg.Add(1)
			s.mu.Unlock()
			go func() { defer s.wg.Done(); defer c.Close(); s.serve(c, connection) }()
		}
	}()
	return s
}

func (s *testServer) serve(conn net.Conn, connection int) {
	s.mu.Lock()
	serverKey := s.key
	if s.rekeyAfter > 0 && connection >= s.rekeyAfter {
		serverKey = s.alternateKey
	}
	rejectAuth := s.rejectAfter > 0 && connection >= s.rejectAfter
	onCommand := s.onCommand
	s.mu.Unlock()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if rejectAuth {
			return nil, fmt.Errorf("denied")
		}
		if meta.User() == "reader" && string(key.Marshal()) == string(s.key.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}, PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if rejectAuth {
			return nil, fmt.Errorf("denied")
		}
		if meta.User() == "reader" && string(password) == "test-password" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(serverKey)
	for _, key := range s.extraKeys {
		cfg.AddHostKey(key)
	}
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
				if onCommand != nil && onCommand(ch, payload.Command) {
					return
				}
				switch payload.Command {
				case "LC_ALL=C slow":
					for range reqs {
					}
					return
				case "LC_ALL=C flood":
					_, _ = ch.Write([]byte(strings.Repeat("x", maxOutput+1)))
				case "LC_ALL=C missing":
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{127}))
					return
				case "LC_ALL=C not-executable":
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{126}))
					return
				case "LC_ALL=C sudo -n mlxfwmanager --query":
					_, _ = ch.Stderr().Write([]byte("sh: 1: sudo: not found"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{127}))
					return
				case "LC_ALL=C sudo -S -p '' nv show bgp":
					input, readErr := io.ReadAll(ch)
					if readErr != nil || string(input) != "sudo-secret\n" {
						_, _ = ch.Stderr().Write([]byte("invalid input"))
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
						return
					}
					_, _ = ch.Write([]byte("switch-state\n"))
				case "LC_ALL=C sudo -S -p '' fail":
					input, _ := io.ReadAll(ch)
					_, _ = ch.Stderr().Write(append([]byte("rejected "), input...))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{7}))
					return
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

func resolvedTestAccess(server *testServer, knownHosts []byte) (*config.SSHAccess, error) {
	dir, err := os.MkdirTemp("", "l8k-ssh-access-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), knownHosts, 0600); err != nil {
		return nil, err
	}
	previous, existed := os.LookupEnv("L8K_SSHPROBE_TEST_PASSWORD")
	if err := os.Setenv("L8K_SSHPROBE_TEST_PASSWORD", "test-password"); err != nil {
		return nil, err
	}
	defer func() {
		if existed {
			_ = os.Setenv("L8K_SSHPROBE_TEST_PASSWORD", previous)
		} else {
			_ = os.Unsetenv("L8K_SSHPROBE_TEST_PASSWORD")
		}
	}()
	document := `credentials:
  reader: {username: reader, passwordRef: {env: L8K_SSHPROBE_TEST_PASSWORD}}
discovery:
  hosts:
    - credentialRef: reader
      ssh: {address: ` + server.listener.Addr().String() + `, knownHostsFile: known_hosts, sudo: true}
  probes: {mode: inventory}
`
	in, err := config.Decode([]byte(document), filepath.Join(dir, "site.yaml"))
	if err != nil {
		return nil, err
	}
	resolved, err := config.Resolve(in)
	if err != nil {
		return nil, err
	}
	return resolved.Targets[0].SSH, nil
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
	It("opens a reusable authenticated read session and sends secret input only on stdin", func() {
		access, err := resolvedTestAccess(server, server.auth().knownHosts)
		Expect(err).NotTo(HaveOccurred())
		session, err := OpenReadSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer session.Close()

		command := "sudo -S -p '' nv show bgp"
		out, err := session.RunWithInput(context.Background(), "switch BGP", command, []byte("sudo-secret\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("switch-state\n"))
		Expect(command).NotTo(ContainSubstring("sudo-secret"))
	})
	It("bounds stdin and guards nil session inputs", func() {
		_, err := OpenReadSession(context.Background(), "ssh://127.0.0.1:22", nil, time.Second)
		Expect(err).To(HaveOccurred())
		wrongTrust := []byte(knownhosts.Line([]string{"other.example"}, server.key.PublicKey()) + "\n")
		access, err := resolvedTestAccess(server, wrongTrust)
		Expect(err).NotTo(HaveOccurred())
		_, err = OpenReadSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
		Expect(Code(err)).To(Equal("SSH_HOST_KEY_REJECTED"))

		access, err = resolvedTestAccess(server, server.auth().knownHosts)
		Expect(err).NotTo(HaveOccurred())
		session, err := OpenReadSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer session.Close()
		_, err = session.RunWithInput(context.Background(), "switch", "query", bytes.Repeat([]byte("x"), maxInput+1))
		Expect(Code(err)).To(Equal("SSH_CONFIGURATION_INVALID"))
	})
	It("redacts stdin from command failures and reconnects after a command timeout", func() {
		access, err := resolvedTestAccess(server, server.auth().knownHosts)
		Expect(err).NotTo(HaveOccurred())
		session, err := OpenReadSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, 120*time.Millisecond)
		Expect(err).NotTo(HaveOccurred())
		defer session.Close()

		_, err = session.RunWithInput(context.Background(), "switch", "sudo -S -p '' fail", []byte("hidden-secret\n"))
		Expect(Code(err)).To(Equal("SSH_COMMAND_FAILED"))
		Expect(err.Error()).NotTo(ContainSubstring("hidden-secret"))

		_, err = session.Run(context.Background(), "slow", "slow")
		Expect(Code(err)).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
		out, err := session.Run(context.Background(), "query", "query")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("observed\n"))
	})
	It("honors caller cancellation without reconnecting", func() {
		access, err := resolvedTestAccess(server, server.auth().knownHosts)
		Expect(err).NotTo(HaveOccurred())
		session, err := OpenReadSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer session.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = session.Run(ctx, "slow", "slow")
		Expect(Code(err)).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
		_, err = session.Run(context.Background(), "after-cancel", "query")
		Expect(Code(err)).To(Equal("SSH_COMMAND_FAILED"))
	})
	It("accepts hashed known-hosts entries through the upstream parser", func() {
		auth := server.auth()
		auth.knownHosts = []byte(knownhosts.HashHostname(knownhosts.Normalize(server.listener.Addr().String())) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(server.key.PublicKey()))) + "\n")
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).NotTo(HaveOccurred())
		c.Close()
	})
	It("selects the pinned Ed25519 key on a server also offering ECDSA and RSA", func() {
		ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
		Expect(err).NotTo(HaveOccurred())
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())
		rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
		Expect(err).NotTo(HaveOccurred())
		server.close()
		server = startServer(ecdsaSigner, rsaSigner)
		for _, hashed := range []bool{false, true} {
			auth := server.auth()
			if hashed {
				auth.knownHosts = []byte(knownhosts.HashHostname(knownhosts.Normalize(server.listener.Addr().String())) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(server.key.PublicKey()))) + "\n")
			}
			auth.knownHosts = append([]byte(knownhosts.Line([]string{"other.example"}, ecdsaSigner.PublicKey())+"\n"), bytes.TrimSpace(auth.knownHosts)...)
			auth.knownHosts = append(auth.knownHosts, []byte(" pinned host key with a multi word comment\n")...)
			c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
			Expect(err).NotTo(HaveOccurred())
			c.Close()
		}
		auth := server.auth()
		auth.knownHosts = []byte(knownhosts.Line([]string{server.listener.Addr().String()}, rsaSigner.PublicKey()) + "\n")
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).NotTo(HaveOccurred())
		c.Close()
	})
	It("preserves host certificate authority verification", func() {
		_, caPrivate, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		ca, err := ssh.NewSignerFromKey(caPrivate)
		Expect(err).NotTo(HaveOccurred())
		certificate := &ssh.Certificate{Key: server.key.PublicKey(), CertType: ssh.HostCert, ValidPrincipals: []string{"127.0.0.1"}, ValidAfter: uint64(time.Now().Add(-time.Minute).Unix()), ValidBefore: uint64(time.Now().Add(time.Minute).Unix())}
		Expect(certificate.SignCert(rand.Reader, ca)).To(Succeed())
		signer, err := ssh.NewCertSigner(certificate, server.key)
		Expect(err).NotTo(HaveOccurred())
		server.close()
		server = startServer(signer)
		auth := server.auth()
		auth.knownHosts = []byte("@cert-authority " + knownhosts.Line([]string{server.listener.Addr().String()}, ca.PublicKey()) + "\n")
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(err).NotTo(HaveOccurred())
		c.Close()
	})
	It("does not promote revoked pinned keys", func() {
		auth := server.auth()
		auth.knownHosts = append(auth.knownHosts, []byte("@revoked "+knownhosts.Line([]string{server.listener.Addr().String()}, server.key.PublicKey())+"\n")...)
		_, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), auth, time.Second)
		Expect(Code(err)).To(Equal("SSH_HOST_KEY_REJECTED"))
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
	It("distinguishes missing executables and missing sudo without remote output", func() {
		c, err := connect(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
		Expect(err).NotTo(HaveOccurred())
		defer c.Close()
		for command, code := range map[string]string{"missing": "TOOL_NOT_AVAILABLE", "not-executable": "TOOL_NOT_EXECUTABLE", "sudo -n mlxfwmanager --query": "TOOL_NOT_AVAILABLE"} {
			output, err := c.Run(context.Background(), "query", command)
			Expect(output).To(BeEmpty())
			Expect(Code(err)).To(Equal(code))
			if strings.HasPrefix(command, "sudo") {
				Expect(err.(*Error).Tool).To(Equal("sudo"))
			}
		}
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
