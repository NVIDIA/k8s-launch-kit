// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestExternalRebootSession(t *testing.T) {
	RegisterTestingT(t)
	server := startServer()
	t.Cleanup(server.close)
	access, err := resolvedTestAccess(server, server.auth().knownHosts)
	Expect(err).NotTo(HaveOccurred())

	var mu sync.Mutex
	commands := []string{}
	server.mu.Lock()
	server.onCommand = func(ch ssh.Channel, command string) bool {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		var output string
		switch command {
		case "LC_ALL=C sudo -S -p '' dmidecode -s system-uuid":
			output = testUUID + "\n"
		case "LC_ALL=C cat /proc/sys/kernel/random/boot_id":
			output = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee\n"
		case "LC_ALL=C sudo -S -p '' lspci -D -vv -s 0000:21:00.0":
			output = " [SN] Serial number: " + testSerial + "\n"
		case "LC_ALL=C sudo -S -p '' systemctl reboot":
			output = ""
		default:
			return false
		}
		if strings.HasPrefix(command, "LC_ALL=C sudo -S -p '' ") {
			input, err := io.ReadAll(ch)
			if err != nil || string(input) != "test-password\n" {
				return false
			}
		}
		if output != "" {
			_, _ = ch.Write([]byte(output))
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return true
	}
	server.mu.Unlock()

	session, err := OpenExternalRebootSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
	Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = session.Close() })
	Expect(session.RebootSystemLevel(context.Background())).To(HaveOccurred())
	identity, err := session.ObserveHost(context.Background(), "0000:21:00.0")
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("ObserveHost() error: %v; commands=%q", err, commands)
	}
	Expect(identity).To(Equal(HostIdentity{SystemUUID: testUUID, AdapterSerial: testSerial, BootID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}))
	_, err = session.ObserveHost(context.Background(), "0000:21:00.0; reboot")
	Expect(Code(err)).To(Equal("SSH_CONFIGURATION_INVALID"))
	actionCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	Expect(session.RebootSystemLevel(actionCtx)).To(Succeed())
	Expect(session.RebootSystemLevel(context.Background())).To(HaveOccurred())

	mu.Lock()
	defer mu.Unlock()
	Expect(commands).To(Equal([]string{
		"LC_ALL=C sudo -S -p '' dmidecode -s system-uuid",
		"LC_ALL=C cat /proc/sys/kernel/random/boot_id",
		"LC_ALL=C sudo -S -p '' lspci -D -vv -s 0000:21:00.0",
		"LC_ALL=C sudo -S -p '' systemctl reboot",
	}))
}

func TestExternalRebootSessionRejectsChangedHostTrust(t *testing.T) {
	RegisterTestingT(t)
	server := startServer()
	t.Cleanup(server.close)
	wrongTrust := []byte(knownhosts.Line([]string{"other.example"}, server.key.PublicKey()) + "\n")
	access, err := resolvedTestAccess(server, wrongTrust)
	Expect(err).NotTo(HaveOccurred())
	_, err = OpenExternalRebootSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
	Expect(Code(err)).To(Equal("SSH_HOST_KEY_REJECTED"))
}

func TestExternalRebootExpiresWhileOpeningCommandChannelWithoutExec(t *testing.T) {
	RegisterTestingT(t)
	server := startServer()
	t.Cleanup(server.close)
	access, err := resolvedTestAccess(server, server.auth().knownHosts)
	Expect(err).NotTo(HaveOccurred())
	var channels atomic.Int32
	var reboots atomic.Int32
	blocked, release := make(chan struct{}), make(chan struct{})
	server.mu.Lock()
	server.beforeSession = func() {
		if channels.Add(1) == 4 {
			close(blocked)
			<-release
		}
	}
	server.onCommand = func(ch ssh.Channel, command string) bool {
		var output string
		switch command {
		case "LC_ALL=C sudo -S -p '' dmidecode -s system-uuid":
			output = testUUID + "\n"
		case "LC_ALL=C cat /proc/sys/kernel/random/boot_id":
			output = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee\n"
		case "LC_ALL=C sudo -S -p '' lspci -D -vv -s 0000:21:00.0":
			output = " [SN] Serial number: " + testSerial + "\n"
		case "LC_ALL=C sudo -S -p '' systemctl reboot":
			reboots.Add(1)
		default:
			return false
		}
		if strings.Contains(command, "sudo -S") {
			_, _ = io.ReadAll(ch)
		}
		_, _ = ch.Write([]byte(output))
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return true
	}
	server.mu.Unlock()
	session, err := OpenExternalRebootSession(context.Background(), "ssh://"+server.listener.Addr().String(), access, time.Second)
	Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = session.Close() })
	_, err = session.ObserveHost(context.Background(), "0000:21:00.0")
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- session.RebootSystemLevel(ctx) }()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("reboot never reached channel setup")
	}
	<-ctx.Done()
	close(release)
	select {
	case err := <-result:
		Expect(Code(err)).To(Equal("SSH_REBOOT_NOT_STARTED"))
	case <-time.After(time.Second):
		t.Fatal("expired channel setup did not return")
	}
	Expect(reboots.Load()).To(BeZero())
}
