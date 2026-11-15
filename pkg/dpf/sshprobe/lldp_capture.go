// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const (
	hostLLDPCaptureWindow  = 65 * time.Second
	hostLLDPCaptureTimeout = 80 * time.Second
	hostLLDPPacketLimit    = 32
	hostLLDPSnapLength     = 2048
	maxHostLLDPCapture     = 1 << 20 // Also bounded by the shared SSH output limit.
)

var hostInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// CaptureHostLLDP captures only the exact selected adapter functions. It
// rechecks host UUID, adapter VPD serial, PCI mapping, and physical port name
// around each capture. Raw pcap bytes are never returned or persisted.
func (Collector) CaptureHostLLDP(ctx context.Context, target config.Target, request inventory.HostLLDPCaptureRequest, timeout time.Duration) (capture inventory.HostLLDPCapture, err error) {
	if ctx == nil || target.Role != "hosts" || target.SSH == nil || !validUUID(request.HostUUID) || request.AdapterKey == "" || request.AdapterSerial == "" || !externalPCIAddress.MatchString(request.AdapterPCIAddress) || timeout <= 0 || !validHostLLDPCapturePorts(request.Ports, request.AdapterPCIAddress) {
		return inventory.HostLLDPCapture{}, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "host-lldp-request"}
	}
	user, password := target.SSH.Credentials()
	key, knownHosts := target.SSH.PrivateKey(), target.SSH.KnownHosts()
	auth := credentials{user: user, password: password, key: key, knownHosts: knownHosts}
	remote, err := newReconnectingRunner(ctx, target.Endpoint, auth, timeout)
	clear(key)
	clear(knownHosts)
	auth.password = ""
	clear(auth.key)
	clear(auth.knownHosts)
	if err != nil {
		return inventory.HostLLDPCapture{}, err
	}
	defer func() {
		if closeErr := remote.Close(); closeErr != nil && err == nil {
			capture = inventory.HostLLDPCapture{}
			err = &Error{Code: "CLEANUP_INCOMPLETE", Stage: "host-lldp-session"}
		}
	}()
	session := hostLLDPSession{runner: remote, access: target.SSH}
	if err := session.verifyIdentity(ctx, request); err != nil {
		return inventory.HostLLDPCapture{}, err
	}
	results := make([]inventory.HostLLDPCaptureResult, 0, len(request.Ports))
	physicalPorts := make(map[string]string, len(request.Ports))
	for _, port := range request.Ports {
		result := inventory.HostLLDPCaptureResult{PCIAddress: strings.ToLower(port.PCIAddress), Interface: port.Interface, State: "unknown", ReasonCode: "HOST_LLDP_UNAVAILABLE", Message: "No bounded LLDP capture was completed on the selected host port.", Neighbors: []inventory.HostLLDPNeighbor{}}
		physicalPort, err := session.verifyPort(ctx, port)
		if err != nil {
			result.ReasonCode = Code(err)
			result.Message = "The selected host interface could not be bound to its requested PCI function and physical port."
			results = append(results, result)
			continue
		}
		result.PhysicalPortName = physicalPort
		physicalPorts[port.Interface] = physicalPort
		if physicalPort != "p0" && physicalPort != "p1" {
			result.ReasonCode = "HOST_PHYSICAL_PORT_UNSUPPORTED"
			result.Message = "The selected host PCI function does not expose a supported p0 or p1 physical port name."
			results = append(results, result)
			continue
		}
		pcap, captureErr := session.capture(ctx, port.Interface)
		if captureErr != nil {
			result.ReasonCode = Code(captureErr)
			result.Message = "The selected host interface could not complete the bounded tcpdump LLDP read."
			results = append(results, result)
			continue
		}
		neighbors, parseErr := parseLLDPCapture(pcap)
		clear(pcap)
		if parseErr != nil {
			result.ReasonCode = "HOST_LLDP_CAPTURE_INVALID"
			result.Message = "The bounded LLDP capture was malformed or exceeded parser limits."
			results = append(results, result)
			continue
		}
		result.Neighbors = neighbors
		switch len(neighbors) {
		case 0:
			result.ReasonCode = "HOST_LLDP_PEER_UNOBSERVED"
			result.Message = "No LLDP peer was observed on the selected host interface during the capture window."
		case 1:
			result.State = "observed"
			result.ReasonCode = "HOST_LLDP_PEER_OBSERVED"
			result.Message = "One unique LLDP peer was observed on the selected host interface."
		default:
			result.State = "failed"
			result.ReasonCode = "HOST_LLDP_PEER_AMBIGUOUS"
			result.Message = "Conflicting LLDP peers were observed on the selected host interface."
		}
		results = append(results, result)
	}
	if err := session.verifyIdentity(ctx, request); err != nil {
		return inventory.HostLLDPCapture{}, err
	}
	for _, port := range request.Ports {
		observedPort, wasVerified := physicalPorts[port.Interface]
		if !wasVerified {
			continue
		}
		physicalPort, err := session.verifyPort(ctx, port)
		if err != nil {
			return inventory.HostLLDPCapture{}, err
		}
		if physicalPort != observedPort {
			return inventory.HostLLDPCapture{}, &Error{Code: "INVENTORY_CHANGED", Stage: "host-lldp-physical-port-recheck"}
		}
	}
	if err := ctx.Err(); err != nil {
		return inventory.HostLLDPCapture{}, &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: "host-lldp-recheck"}
	}
	portNames := map[string]bool{}
	for _, result := range results {
		if result.PhysicalPortName == "p0" || result.PhysicalPortName == "p1" {
			if portNames[result.PhysicalPortName] {
				for i := range results {
					if results[i].PhysicalPortName == result.PhysicalPortName {
						results[i].State = "failed"
						results[i].ReasonCode = "HOST_PHYSICAL_PORT_AMBIGUOUS"
						results[i].Message = "Multiple selected host interfaces report the same p0 or p1 physical port name."
					}
				}
				continue
			}
			portNames[result.PhysicalPortName] = true
		}
	}
	return inventory.HostLLDPCapture{Ports: results}, nil
}

func validHostLLDPCapturePorts(ports []inventory.HostLLDPCapturePort, adapterPCI string) bool {
	if len(ports) != 2 {
		return false
	}
	seenBDF, seenInterface := map[string]bool{}, map[string]bool{}
	for _, port := range ports {
		bdf := strings.ToLower(port.PCIAddress)
		if !externalPCIAddress.MatchString(port.PCIAddress) || !hostInterfaceName.MatchString(port.Interface) || seenBDF[bdf] || seenInterface[port.Interface] {
			return false
		}
		if pciSlot(bdf) != pciSlot(strings.ToLower(adapterPCI)) {
			return false
		}
		seenBDF[bdf], seenInterface[port.Interface] = true, true
	}
	return true
}

func pciSlot(bdf string) string {
	if dot := strings.LastIndexByte(bdf, '.'); dot >= 0 {
		return bdf[:dot]
	}
	return ""
}

type hostLLDPSession struct {
	runner *reconnectingRunner
	access *config.SSHAccess
}

func (s hostLLDPSession) run(ctx context.Context, stage, command string) (string, error) {
	return s.runWithTimeout(ctx, stage, command, nil, s.runner.timeout)
}

func (s hostLLDPSession) runWithTimeout(ctx context.Context, stage, command string, input []byte, timeout time.Duration) (string, error) {
	if !s.access.Sudo {
		return s.runner.RunWithInputTimeout(ctx, stage, command, input, timeout)
	}
	_, password := s.access.Credentials()
	key := s.access.PrivateKey()
	defer clear(key)
	if len(key) == 0 && password != "" {
		if len(input) != 0 {
			return "", &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: stage}
		}
		sudoInput := []byte(password + "\n")
		defer clear(sudoInput)
		return s.runner.RunWithInputTimeout(ctx, stage, "sudo -S -p '' "+command, sudoInput, timeout)
	}
	return s.runner.RunWithInputTimeout(ctx, stage, "sudo -n "+command, input, timeout)
}

func (s hostLLDPSession) verifyIdentity(ctx context.Context, request inventory.HostLLDPCaptureRequest) error {
	uuid, err := s.run(ctx, "host-lldp-uuid", "dmidecode -s system-uuid")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(uuid), request.HostUUID) {
		return &Error{Code: "INVENTORY_CHANGED", Stage: "host-lldp-uuid"}
	}
	vpdText, err := s.run(ctx, "host-lldp-vpd", "lspci -D -vv -s "+strings.ToLower(request.AdapterPCIAddress))
	if err != nil {
		return err
	}
	serial, _, err := vpd(vpdText)
	if err != nil || serial != request.AdapterSerial {
		return &Error{Code: "INVENTORY_CHANGED", Stage: "host-lldp-vpd"}
	}
	return nil
}

func (s hostLLDPSession) verifyPort(ctx context.Context, port inventory.HostLLDPCapturePort) (string, error) {
	base := "/sys/class/net/" + port.Interface
	device, err := s.run(ctx, "host-lldp-pci", "readlink -f "+base+"/device")
	if err != nil {
		return "", err
	}
	if path.Base(strings.TrimSpace(device)) != strings.ToLower(port.PCIAddress) {
		return "", &Error{Code: "INVENTORY_CHANGED", Stage: "host-lldp-pci"}
	}
	physicalPort, err := s.run(ctx, "host-lldp-physical-port", "cat "+base+"/phys_port_name")
	if err != nil {
		return "", err
	}
	physicalPort = strings.TrimSpace(physicalPort)
	if physicalPort == "" || strings.ContainsAny(physicalPort, "\r\n\x00") {
		return "", &Error{Code: "SSH_RUNTIME_INVALID", Stage: "host-lldp-physical-port"}
	}
	return physicalPort, nil
}

func (s hostLLDPSession) capture(ctx context.Context, iface string) ([]byte, error) {
	if _, err := s.run(ctx, "host-lldp-tools", "sh -c 'command -v timeout >/dev/null && command -v tcpdump >/dev/null'"); err != nil {
		return nil, err
	}
	command := fmt.Sprintf("timeout --preserve-status --signal=INT --kill-after=1s %ds tcpdump -p -U -c %d -s %d -i %s -w - 'ether proto 0x88cc'", int64(hostLLDPCaptureWindow/time.Second), hostLLDPPacketLimit, hostLLDPSnapLength, iface)
	output, err := s.runWithTimeout(ctx, "host-lldp-capture", command, nil, hostLLDPCaptureTimeout)
	if err != nil {
		return nil, err
	}
	if len(output) > maxHostLLDPCapture {
		return nil, &Error{Code: "SSH_OUTPUT_LIMIT", Stage: "host-lldp-capture"}
	}
	return []byte(output), nil
}

type pcapByteOrder struct {
	order binary.ByteOrder
	nano  bool
}

func parseLLDPCapture(data []byte) ([]inventory.HostLLDPNeighbor, error) {
	if len(data) < 24 || len(data) > maxHostLLDPCapture {
		return nil, fmt.Errorf("invalid pcap length")
	}
	var pcap pcapByteOrder
	switch string(data[:4]) {
	case "\xd4\xc3\xb2\xa1":
		pcap.order = binary.LittleEndian
	case "\xa1\xb2\xc3\xd4":
		pcap.order = binary.BigEndian
	case "\x4d\x3c\xb2\xa1":
		pcap.order, pcap.nano = binary.LittleEndian, true
	case "\xa1\xb2\x3c\x4d":
		pcap.order, pcap.nano = binary.BigEndian, true
	default:
		return nil, fmt.Errorf("unsupported pcap magic")
	}
	if pcap.order.Uint16(data[4:6]) != 2 || pcap.order.Uint16(data[6:8]) != 4 || pcap.order.Uint32(data[20:24]) != 1 {
		return nil, fmt.Errorf("unsupported pcap version or link type")
	}
	snaplen := pcap.order.Uint32(data[16:20])
	if snaplen == 0 || snaplen > hostLLDPSnapLength {
		return nil, fmt.Errorf("pcap snap length exceeds bound")
	}
	frames := 0
	type observedNeighbor struct {
		value    inventory.HostLLDPNeighbor
		expires  uint64
		observed uint64
	}
	unique := map[string]observedNeighbor{}
	latestTimestamp := uint64(0)
	for offset := 24; offset < len(data); {
		if len(data)-offset < 16 || frames >= hostLLDPPacketLimit {
			return nil, fmt.Errorf("pcap packet header or count exceeds bound")
		}
		captured := pcap.order.Uint32(data[offset+8 : offset+12])
		original := pcap.order.Uint32(data[offset+12 : offset+16])
		seconds := pcap.order.Uint32(data[offset : offset+4])
		fraction := pcap.order.Uint32(data[offset+4 : offset+8])
		fractionLimit := uint32(1_000_000)
		if pcap.nano {
			fractionLimit = 1_000_000_000
		}
		offset += 16
		if fraction >= fractionLimit || captured > snaplen || captured > uint32(len(data)-offset) || original < captured {
			return nil, fmt.Errorf("invalid pcap packet length")
		}
		timestamp := uint64(seconds)*1_000_000_000 + uint64(fraction)
		if !pcap.nano {
			timestamp = uint64(seconds)*1_000_000_000 + uint64(fraction)*1_000
		}
		if timestamp > latestTimestamp {
			latestTimestamp = timestamp
		}
		frame := data[offset : offset+int(captured)]
		offset += int(captured)
		frames++
		neighbor, ttl, found, err := parseLLDPFrame(frame)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		key := lldpNeighborKey(neighbor)
		if ttl == 0 {
			if previous, exists := unique[key]; !exists || previous.observed <= timestamp {
				delete(unique, key)
			}
			continue
		}
		if previous, exists := unique[key]; !exists || previous.observed <= timestamp {
			unique[key] = observedNeighbor{value: neighbor, expires: timestamp + uint64(ttl)*1_000_000_000, observed: timestamp}
		}
	}
	keys := make([]string, 0, len(unique))
	for key, observation := range unique {
		if observation.expires >= latestTimestamp {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	out := make([]inventory.HostLLDPNeighbor, 0, len(keys))
	for _, key := range keys {
		out = append(out, unique[key].value)
	}
	return out, nil
}

func lldpNeighborKey(neighbor inventory.HostLLDPNeighbor) string {
	return strings.Join([]string{fmt.Sprint(neighbor.ChassisIDSubtype), neighbor.ChassisID, fmt.Sprint(neighbor.PortIDSubtype), neighbor.PortID}, "\x00")
}

func parseLLDPFrame(frame []byte) (inventory.HostLLDPNeighbor, uint16, bool, error) {
	var neighbor inventory.HostLLDPNeighbor
	if len(frame) < 14 {
		return neighbor, 0, false, fmt.Errorf("truncated ethernet frame")
	}
	source, err := net.ParseMAC(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", frame[6], frame[7], frame[8], frame[9], frame[10], frame[11]))
	if err != nil || len(source) != 6 || source[0]&1 != 0 || source.String() == "00:00:00:00:00:00" {
		return neighbor, 0, false, fmt.Errorf("invalid ethernet source mac")
	}
	if !bytes.Equal(frame[:6], []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}) {
		return neighbor, 0, false, fmt.Errorf("invalid lldp destination mac")
	}
	neighbor.SourceMAC = source.String()
	etherType, offset := binary.BigEndian.Uint16(frame[12:14]), 14
	for i := 0; i < 2 && (etherType == 0x8100 || etherType == 0x88a8 || etherType == 0x9100); i++ {
		if len(frame)-offset < 4 {
			return neighbor, 0, false, fmt.Errorf("truncated vlan header")
		}
		etherType = binary.BigEndian.Uint16(frame[offset+2 : offset+4])
		offset += 4
	}
	if etherType != 0x88cc {
		return neighbor, 0, false, nil
	}
	frame = frame[offset:]
	seen := map[uint8]bool{}
	position := uint8(0)
	ended := false
	ttl := uint16(0)
	for len(frame) >= 2 {
		header := binary.BigEndian.Uint16(frame[:2])
		typeID, length := uint8(header>>9), int(header&0x1ff)
		frame = frame[2:]
		if length > len(frame) {
			return neighbor, 0, false, fmt.Errorf("truncated lldp tlv")
		}
		value := frame[:length]
		frame = frame[length:]
		if typeID == 0 {
			if length != 0 || position < 3 {
				return neighbor, 0, false, fmt.Errorf("invalid lldp end tlv")
			}
			for _, padding := range frame {
				if padding != 0 {
					return neighbor, 0, false, fmt.Errorf("invalid nonzero ethernet padding after lldp end tlv")
				}
			}
			ended = true
			break
		}
		if typeID > 3 && position < 3 {
			return neighbor, 0, false, fmt.Errorf("optional lldp tlv precedes mandatory identity tlvs")
		}
		if typeID <= 3 {
			position++
			if typeID != position || seen[typeID] {
				return neighbor, 0, false, fmt.Errorf("invalid mandatory lldp tlv order")
			}
		}
		if typeID == 1 || typeID == 2 || typeID == 4 || typeID == 5 {
			if seen[typeID] {
				return neighbor, 0, false, fmt.Errorf("duplicate lldp identity tlv")
			}
			seen[typeID] = true
		}
		switch typeID {
		case 1:
			if len(value) < 2 {
				return neighbor, 0, false, fmt.Errorf("invalid chassis id tlv")
			}
			neighbor.ChassisIDSubtype = value[0]
			if value[0] != 4 || len(value) != 7 || value[1]&1 != 0 || bytes.Equal(value[1:], []byte{0, 0, 0, 0, 0, 0}) {
				return neighbor, 0, false, fmt.Errorf("unsupported chassis id type")
			}
			neighbor.ChassisID = net.HardwareAddr(value[1:]).String()
		case 2:
			if len(value) < 2 {
				return neighbor, 0, false, fmt.Errorf("invalid port id tlv")
			}
			neighbor.PortIDSubtype = value[0]
			if value[0] != 5 {
				return neighbor, 0, false, fmt.Errorf("unsupported port id type")
			}
			neighbor.PortID, err = lldpText(value[1:])
		case 3:
			if len(value) != 2 {
				return neighbor, 0, false, fmt.Errorf("invalid ttl tlv")
			}
			ttl = binary.BigEndian.Uint16(value)
		case 4:
			neighbor.PortDescription, err = lldpText(value)
		case 5:
			neighbor.SystemName, err = lldpText(value)
		}
		if err != nil {
			return neighbor, 0, false, err
		}
	}
	if !ended || !seen[1] || !seen[2] || position < 3 || neighbor.ChassisID == "" || neighbor.PortID == "" {
		return neighbor, 0, false, fmt.Errorf("incomplete lldp identity")
	}
	return neighbor, ttl, true, nil
}

func lldpText(value []byte) (string, error) {
	if len(value) == 0 || len(value) > 256 || !utf8.Valid(value) {
		return "", fmt.Errorf("invalid lldp identity length")
	}
	if !bytes.Equal(bytes.Trim(value, " \t"), value) || bytes.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("invalid lldp identity text")
	}
	return string(value), nil
}
