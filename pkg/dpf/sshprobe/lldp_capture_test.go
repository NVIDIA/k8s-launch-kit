// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
)

func TestParseLLDPCaptureDeduplicatesSamePeerAndKeepsConflicts(t *testing.T) {
	frame := testLLDPFrame("swp19s0", false, 120)
	pcap := testPCAP(frame, frame, testLLDPFrame("swp19s1", false, 120))
	got, err := parseLLDPCapture(pcap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d unique neighbors, want 2: %#v", len(got), got)
	}
	if got[0].ChassisIDSubtype != 4 || got[0].ChassisID != "cc:40:f3:55:e1:2c" || got[0].PortIDSubtype != 5 || got[0].PortID != "swp19s0" || got[0].SystemName != "leaf-a" || got[0].SourceMAC != "20:4d:52:3e:9a:b8" {
		t.Fatalf("unexpected parsed neighbor: %#v", got[0])
	}
}

func TestParseLLDPCaptureSupportsVLANAndIgnoresWithdrawnPeer(t *testing.T) {
	got, err := parseLLDPCapture(testPCAP(testLLDPFrame("swp1", true, 120), testLLDPFrame("swp1", false, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("TTL-zero withdrawal left a stale peer: %#v", got)
	}
}

func TestParseLLDPCaptureHonorsEndPaddingMandatoryOrderAndTTL(t *testing.T) {
	padded := padTestFrame(testLLDPFrame("p0", false, 120), 60)
	stale := testLLDPFrame("swp-stale", false, 1)
	current := testLLDPFrame("swp-current", false, 120)
	got, err := parseLLDPCapture(testPCAPAt([][]byte{padded, stale, current}, []uint32{1, 1, 3}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PortID != "p0" || got[1].PortID != "swp-current" {
		t.Fatalf("got %#v, want the padded live peers with expired peer removed", got)
	}

	badPadding := padTestFrame(testLLDPFrame("swp1", false, 120), 60)
	badPadding[len(badPadding)-1] = 1
	if _, err := parseLLDPCapture(testPCAP(badPadding)); err == nil {
		t.Fatal("nonzero bytes after the LLDP end TLV were accepted as Ethernet padding")
	}

	optionalFirst := []byte{0x01, 0x80, 0xc2, 0, 0, 0x0e, 0x20, 0x4d, 0x52, 0x3e, 0x9a, 0xb8, 0x88, 0xcc}
	optionalFirst = appendTLV(optionalFirst, 5, []byte("leaf"))
	optionalFirst = appendTLV(optionalFirst, 1, append([]byte{4}, []byte{0xcc, 0x40, 0xf3, 0x55, 0xe1, 0x2c}...))
	optionalFirst = appendTLV(optionalFirst, 2, append([]byte{5}, []byte("swp1")...))
	optionalFirst = appendTLV(optionalFirst, 3, []byte{0, 120})
	optionalFirst = appendTLV(optionalFirst, 0, nil)
	if _, err := parseLLDPCapture(testPCAP(optionalFirst)); err == nil {
		t.Fatal("optional TLV before mandatory identity TLVs was accepted")
	}
}

func TestParseLLDPCaptureRejectsMalformedOrOutOfScopeInput(t *testing.T) {
	valid := testPCAP(testLLDPFrame("swp1", false, 120))
	tests := []struct {
		name string
		data []byte
	}{
		{name: "truncated global header", data: valid[:12]},
		{name: "truncated packet", data: valid[:len(valid)-1]},
		{name: "oversized snaplen", data: func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[16:20], hostLLDPSnapLength+1)
			return b
		}()},
		{name: "unsupported pcap link", data: func() []byte {
			b := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint32(b[20:24], 113)
			return b
		}()},
		{name: "bad chassis subtype", data: testPCAP(testLLDPFrameSubtype("swp1", 2, 120))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseLLDPCapture(test.data); err == nil {
				t.Fatal("expected malformed input to fail")
			}
		})
	}
}

func TestValidHostLLDPCapturePortsRequireDistinctFunctionsOnSelectedCard(t *testing.T) {
	ports := []inventory.HostLLDPCapturePort{{PCIAddress: "0000:82:00.0", Interface: "ens5f0np0"}, {PCIAddress: "0000:82:00.1", Interface: "ens5f1np1"}}
	if !validHostLLDPCapturePorts(ports, "0000:82:00.0") {
		t.Fatal("valid selected functions were rejected")
	}
	ports[1].PCIAddress = "0000:83:00.1"
	if validHostLLDPCapturePorts(ports, "0000:82:00.0") {
		t.Fatal("function from a different card was accepted")
	}
	ports[1] = inventory.HostLLDPCapturePort{PCIAddress: "0000:82:00.0", Interface: "ens5f1np1"}
	if validHostLLDPCapturePorts(ports, "0000:82:00.0") {
		t.Fatal("duplicate PCI function was accepted")
	}
}

var _ = Describe("selected-host LLDP capture", func() {
	It("uses pinned SSH trust and auth, validates selected PCI mappings, and returns parsed peers only", func() {
		server := startServer()
		DeferCleanup(server.close)
		access, err := resolvedTestAccess(server, server.auth().knownHosts)
		Expect(err).NotTo(HaveOccurred())
		var mu sync.Mutex
		commands := []string{}
		server.mu.Lock()
		server.onCommand = func(channel ssh.Channel, command string) bool {
			mu.Lock()
			commands = append(commands, command)
			mu.Unlock()
			if strings.Contains(command, "sudo -S -p ''") {
				input, readErr := io.ReadAll(channel)
				if readErr != nil || string(input) != "test-password\n" {
					_, _ = channel.Stderr().Write([]byte("invalid sudo input"))
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
					return true
				}
			}
			out, status := "", uint32(0)
			switch {
			case strings.HasSuffix(command, "dmidecode -s system-uuid"):
				out = testUUID + "\n"
			case strings.HasSuffix(command, "lspci -D -vv -s 0000:82:00.0"):
				out = "Physical Slot: 5\n [SN] Serial number: " + testSerial + "\n"
			case strings.HasSuffix(command, "readlink -f /sys/class/net/ens5f0np0/device"):
				out = "/sys/devices/0000:82:00.0\n"
			case strings.HasSuffix(command, "readlink -f /sys/class/net/ens5f1np1/device"):
				out = "/sys/devices/0000:82:00.1\n"
			case strings.HasSuffix(command, "cat /sys/class/net/ens5f0np0/phys_port_name"):
				out = "p0\n"
			case strings.HasSuffix(command, "cat /sys/class/net/ens5f1np1/phys_port_name"):
				out = "p1\n"
			case strings.Contains(command, "command -v timeout"):
				out = ""
			case strings.Contains(command, "tcpdump") && strings.Contains(command, "ens5f0np0"):
				_, _ = channel.Stderr().Write([]byte("tcpdump: listening on ens5f0np0\n"))
				out = string(testPCAP(testLLDPFrame("swp19s0", false, 120)))
			case strings.Contains(command, "tcpdump") && strings.Contains(command, "ens5f1np1"):
				_, _ = channel.Stderr().Write([]byte("tcpdump: listening on ens5f1np1\n"))
				out = string(testPCAP(testLLDPFrame("swp19s1", false, 120)))
			default:
				status = 127
			}
			if out != "" {
				_, _ = channel.Write([]byte(out))
			}
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			return true
		}
		server.mu.Unlock()

		target := config.Target{Role: "hosts", Endpoint: "ssh://" + server.listener.Addr().String(), SSH: access}
		request := inventory.HostLLDPCaptureRequest{
			HostUUID: testUUID, AdapterKey: "selected-adapter", AdapterSerial: testSerial, AdapterPCIAddress: "0000:82:00.0",
			Ports: []inventory.HostLLDPCapturePort{{PCIAddress: "0000:82:00.0", Interface: "ens5f0np0"}, {PCIAddress: "0000:82:00.1", Interface: "ens5f1np1"}},
		}
		capture, err := (Collector{}).CaptureHostLLDP(context.Background(), target, request, time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(capture.Ports).To(HaveLen(2))
		Expect(capture.Ports[0].PhysicalPortName).To(Equal("p0"))
		Expect(capture.Ports[0].Neighbors).To(HaveLen(1))
		Expect(capture.Ports[1].PhysicalPortName).To(Equal("p1"))
		Expect(capture.Ports[1].Neighbors[0].PortID).To(Equal("swp19s1"))
		mu.Lock()
		joined := strings.Join(commands, "\n")
		mu.Unlock()
		Expect(joined).To(ContainSubstring("sudo -S -p '' dmidecode -s system-uuid"))
		Expect(joined).To(ContainSubstring("sudo -S -p '' timeout --preserve-status --signal=INT --kill-after=1s 65s tcpdump -p -U -c 32 -s 2048 -i ens5f0np0 -w - 'ether proto 0x88cc'"))
		Expect(joined).NotTo(ContainSubstring("1m5s"), "GNU timeout does not accept Go's composite duration format")
		Expect(joined).NotTo(ContainSubstring("test-password"))
		Expect(fmt.Sprint(capture)).NotTo(ContainSubstring("pcap"))
	})
})

func testLLDPFrame(port string, vlan bool, ttl uint16) []byte {
	frame := []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e, 0x20, 0x4d, 0x52, 0x3e, 0x9a, 0xb8}
	if vlan {
		frame = append(frame, 0x81, 0x00, 0x00, 0x01, 0x88, 0xcc)
	} else {
		frame = append(frame, 0x88, 0xcc)
	}
	frame = appendTLV(frame, 1, append([]byte{4}, []byte{0xcc, 0x40, 0xf3, 0x55, 0xe1, 0x2c}...))
	frame = appendTLV(frame, 2, append([]byte{5}, []byte(port)...))
	ttlBytes := []byte{byte(ttl >> 8), byte(ttl)}
	frame = appendTLV(frame, 3, ttlBytes)
	frame = appendTLV(frame, 5, []byte("leaf-a"))
	frame = appendTLV(frame, 4, []byte(port))
	return appendTLV(frame, 0, nil)
}

func testLLDPFrameSubtype(port string, subtype byte, ttl uint16) []byte {
	frame := []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e, 0x20, 0x4d, 0x52, 0x3e, 0x9a, 0xb8, 0x88, 0xcc}
	frame = appendTLV(frame, 1, append([]byte{subtype}, []byte("chassis")...))
	frame = appendTLV(frame, 2, append([]byte{5}, []byte(port)...))
	frame = appendTLV(frame, 3, []byte{byte(ttl >> 8), byte(ttl)})
	return appendTLV(frame, 0, nil)
}

func appendTLV(frame []byte, kind uint8, value []byte) []byte {
	header := uint16(kind)<<9 | uint16(len(value))
	return append(append(frame, byte(header>>8), byte(header)), value...)
}

func testPCAP(frames ...[]byte) []byte {
	seconds := make([]uint32, len(frames))
	for i := range seconds {
		seconds[i] = uint32(i + 1)
	}
	return testPCAPAt(frames, seconds)
}

func padTestFrame(frame []byte, size int) []byte {
	if len(frame) >= size {
		return frame
	}
	return append(frame, make([]byte, size-len(frame))...)
}

func testPCAPAt(frames [][]byte, seconds []uint32) []byte {
	data := make([]byte, 24)
	copy(data, []byte{0xd4, 0xc3, 0xb2, 0xa1})
	binary.LittleEndian.PutUint16(data[4:6], 2)
	binary.LittleEndian.PutUint16(data[6:8], 4)
	binary.LittleEndian.PutUint32(data[16:20], hostLLDPSnapLength)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	for i, frame := range frames {
		header := make([]byte, 16)
		binary.LittleEndian.PutUint32(header[0:4], seconds[i])
		binary.LittleEndian.PutUint32(header[8:12], uint32(len(frame)))
		binary.LittleEndian.PutUint32(header[12:16], uint32(len(frame)))
		data = append(data, header...)
		data = append(data, frame...)
	}
	return data
}
