// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// ASICPort records the selected physical interface's direct SDK RIF read and
// independently mapped Linux VRF. RouterTable is switchd's table identity for
// the actual SDK RouterID, not an inference from NVUE configuration.
type ASICPort struct {
	Name        string `json:"name" yaml:"name"`
	RIF         uint32 `json:"rif" yaml:"rif"`
	RouterID    uint32 `json:"routerId" yaml:"routerId"`
	RouterTable uint32 `json:"routerTable" yaml:"routerTable"`
	LinuxVRF    string `json:"linuxVRF" yaml:"linuxVRF"`
	LinuxTable  uint32 `json:"linuxTable" yaml:"linuxTable"`
	MAC         string `json:"mac" yaml:"mac"`
	MTU         uint32 `json:"mtu" yaml:"mtu"`
	IPv4Enabled bool   `json:"ipv4Enabled" yaml:"ipv4Enabled"`
	IPv6Enabled bool   `json:"ipv6Enabled" yaml:"ipv6Enabled"`
}

// ASICObservation contains only selected ports from a complete, stable source
// join. Absence means unmeasured; a zero numeric identity is a valid observation.
type ASICObservation struct {
	Ports []ASICPort `json:"ports" yaml:"ports"`
}

// ASICInputs are private, ephemeral command responses. They must never be
// embedded in public inventory or logged.
type ASICInputs struct {
	Before, After ASICIdentityInputs
	RIFs          *ASICRIFDump
}

// ASICIdentityInputs owns one source snapshot; preflight validates Before
// independently, without manufacturing a matching After observation.
type ASICIdentityInputs struct {
	Root, Interfaces, VRFs, Linux, Config []byte
	// Pending distinguishes a confirmed zero-byte diff from a missing (nil) read.
	Pending []byte
}

// ASICBackendTables selects numeric files under switchd's fixed software-table
// directory. IDs are discovered by exact table name, never release constants.
type ASICBackendTables struct{ Interfaces, VRFs uint32 }

var backendRootRow = regexp.MustCompile(`^TableId: ([0-9]+) \| Application: (.+)$`)
var backendVRFRow = regexp.MustCompile(`^ vr-id ([0-9]+) \| table-id ([0-9]+)$`)
var backendPortRow = regexp.MustCompile(`^ ifp entry port: (swp[0-9]+(?:s[0-9]+)?) \(if-index:([0-9]+)\) -$`)

// ParseASICBackendTables validates the complete root dump before selecting files.
func ParseASICBackendTables(raw []byte) (ASICBackendTables, error) {
	lines, err := backendLines(raw, "Root HashTable", false)
	if err != nil {
		return ASICBackendTables{}, err
	}
	var out ASICBackendTables
	names, ids := map[string]bool{}, map[uint32]bool{}
	for _, line := range lines {
		match := backendRootRow.FindStringSubmatch(line)
		if match == nil {
			return out, fmt.Errorf("malformed switchd root row")
		}
		id, err := asicUint(match[1], 10)
		if err != nil || id == 0 || names[match[2]] || ids[id] {
			return out, fmt.Errorf("duplicate or invalid switchd root identity")
		}
		names[match[2]], ids[id] = true, true
		switch match[2] {
		case "Backend Interface Table":
			out.Interfaces = id
		case "Backend VRF Table":
			out.VRFs = id
		}
	}
	if out.Interfaces == 0 || out.VRFs == 0 {
		return out, fmt.Errorf("required switchd tables are absent")
	}
	return out, nil
}

func backendLines(raw []byte, name string, counted bool) ([]string, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, fmt.Errorf("switchd table is absent or exceeds its limit")
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")), "\n")
	if len(lines) < 7 || !separator(lines[0], '=') || !separator(lines[2], '=') || !separator(lines[len(lines)-2], '=') || lines[len(lines)-1] != "TRUE" {
		return nil, fmt.Errorf("switchd table framing is incomplete")
	}
	header := regexp.MustCompile(`^TableId: [0-9]+ - Application: ` + regexp.QuoteMeta(name) + `(?: Count: ([0-9]+))?$`).FindStringSubmatch(lines[1])
	if header == nil || counted && header[1] == "" || !counted && header[1] != "" {
		return nil, fmt.Errorf("unsupported switchd table header")
	}
	footer := len(lines) - 2
	if counted {
		footer = len(lines) - 4
		if !separator(lines[footer], '=') || !regexp.MustCompile(`^TableSize: [0-9]+ Bytes$`).MatchString(lines[footer+1]) {
			return nil, fmt.Errorf("switchd table footer is incomplete")
		}
	}
	return lines[3:footer], nil
}

func separator(s string, char byte) bool {
	if len(s) < 8 {
		return false
	}
	for i := range s {
		if s[i] != char {
			return false
		}
	}
	return true
}

func asicUint(s string, base int) (uint32, error) {
	n, err := strconv.ParseUint(s, base, 32)
	return uint32(n), err
}

func parseASICVRFs(raw []byte) (map[uint32]uint32, error) {
	lines, err := backendLines(raw, "Backend VRF Table", true)
	if err != nil {
		return nil, err
	}
	result := map[uint32]uint32{}
	tables := map[uint32]bool{}
	for _, line := range lines {
		match := backendVRFRow.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("malformed backend VRF row")
		}
		vrid, e1 := asicUint(match[1], 10)
		table, e2 := asicUint(match[2], 10)
		if _, exists := result[vrid]; exists || e1 != nil || e2 != nil || tables[table] {
			return nil, fmt.Errorf("ambiguous backend VRF identity")
		}
		result[vrid] = table
		tables[table] = true
	}
	if len(result) == 0 || len(result) != backendCount(raw) {
		return nil, fmt.Errorf("backend VRF map is empty or incomplete")
	}
	return result, nil
}

type asicInterface struct {
	Index, RIF, VPort, Table uint32
	MAC                      string
}

// Only fields with their exact owner/indentation are interpreted. In particular
// a nested bridge/VLAN field cannot replace the physical interface identity.
func parseASICInterfaces(raw []byte, selected map[string]bool) (map[string]asicInterface, error) {
	lines, err := backendLines(raw, "Backend Interface Table", true)
	if err != nil {
		return nil, err
	}
	result := map[string]asicInterface{}
	var name string
	var value asicInterface
	fields := map[string]bool{}
	section := ""
	finish := func() error {
		if !selected[name] {
			return nil
		}
		for _, key := range []string{"if-entry", "table-id", "mac", "vport-lid", "l3-interface-id"} {
			if !fields[key] {
				return fmt.Errorf("selected backend interface is incomplete")
			}
		}
		if _, exists := result[name]; exists {
			return fmt.Errorf("selected backend interface is duplicated")
		}
		result[name] = value
		return nil
	}
	seenNames := map[string]bool{}
	rowCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ifp entry ") {
			rowCount++
			if err := finish(); err != nil {
				return nil, err
			}
			name = ""
			value = asicInterface{}
			fields = map[string]bool{}
			section = ""
			match := backendPortRow.FindStringSubmatch(line)
			if match == nil {
				continue
			} // Nonphysical interfaces are outside this adapter.
			if seenNames[match[1]] {
				return nil, fmt.Errorf("duplicate backend physical interface")
			}
			seenNames[match[1]] = true
			name = match[1]
			value.Index, err = asicUint(match[2], 10)
			if err != nil || value.Index == 0 {
				return nil, fmt.Errorf("invalid backend ifindex")
			}
			continue
		}
		if !selected[name] {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		depth := len(line) - len(strings.TrimLeft(line, " "))
		owned := false
		switch depth {
		case 3:
			section = ""
			switch key {
			case "if-entry":
				section = "interface"
				owned = true
			case "vport-entry":
				section = "vport"
			case "l3-interface-id":
				owned = true
			}
		case 5:
			owned = section == "interface" && (key == "table-id" || key == "mac") || section == "vport" && key == "vport-lid"
		}
		if !owned {
			continue
		}

		if fields[key] {
			return nil, fmt.Errorf("duplicate selected backend interface field")
		}
		fields[key] = true
		switch key {
		case "if-entry":
			if line != fmt.Sprintf("   if-entry port: %s (if-index:%d) -", name, value.Index) {
				return nil, fmt.Errorf("backend interface owner mismatch")
			}
		case "table-id":
			value.Table, err = asicUint(val, 10)
		case "l3-interface-id":
			value.RIF, err = asicUint(val, 10)
		case "vport-lid":
			value.VPort, err = asicUint(val, 0)
		case "mac":
			value.MAC, err = asicMAC(val)
		}
		if err != nil {
			return nil, fmt.Errorf("malformed selected backend interface field")
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(result) != len(selected) || rowCount != backendCount(raw) {
		return nil, fmt.Errorf("selected backend interface coverage is incomplete")
	}
	return result, nil
}

func asicMAC(s string) (string, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("invalid MAC address")
	}
	return mac.String(), nil
}

type linuxASICPort struct {
	Index, Table uint32
	MAC, VRF     string
}

func parseASICLinux(raw []byte, selected map[string]bool) (map[string]linuxASICPort, error) {
	var rows []struct {
		Index    uint32          `json:"ifindex"`
		Name     string          `json:"ifname"`
		MAC      string          `json:"address"`
		Master   json.RawMessage `json:"master"`
		LinkInfo struct {
			Kind string `json:"info_kind"`
			Data struct {
				Table uint32 `json:"table"`
			} `json:"info_data"`
		} `json:"linkinfo"`
	}
	if len(raw) == 0 || len(raw) > 1<<20 || asicJSON(raw, &rows) != nil || rows == nil {
		return nil, fmt.Errorf("the Linux link identity output is absent or malformed")
	}
	vrfs := map[string]uint32{}
	seen := map[string]bool{}
	indexes := map[uint32]bool{}
	tables := map[uint32]bool{}
	for _, row := range rows {
		if row.Name == "" || seen[row.Name] || row.Index == 0 || indexes[row.Index] {
			return nil, fmt.Errorf("the Linux link identity is ambiguous")
		}
		seen[row.Name] = true
		indexes[row.Index] = true
		if row.LinkInfo.Kind == "vrf" {
			table := row.LinkInfo.Data.Table
			if table == 0 || tables[table] {
				return nil, fmt.Errorf("the Linux VRF table identity is ambiguous")
			}
			vrfs[row.Name] = table
			tables[table] = true
		}
	}
	result := map[string]linuxASICPort{}
	for _, row := range rows {
		if !selected[row.Name] {
			continue
		}
		var master string
		if len(row.Master) > 0 {
			if bytes.Equal(bytes.TrimSpace(row.Master), []byte("null")) || json.Unmarshal(row.Master, &master) != nil || master == "" {
				return nil, fmt.Errorf("selected Linux master identity is malformed")
			}
		}
		table, ok := vrfs[master]
		vrf := master
		// Exact-build Cumulus cl-route-check extract_table uses table 0 for
		// unqualified/default routing. This is a source-defined sentinel, not
		// Linux's numeric main-table ID or a VRID arithmetic relationship.
		if master == "" {
			table = 0
			vrf = "default"
			ok = true
		}
		if !ok {
			return nil, fmt.Errorf("selected interface lacks an explicit Linux VRF table mapping")
		}
		mac, err := asicMAC(row.MAC)
		if err != nil {
			return nil, err
		}
		result[row.Name] = linuxASICPort{Index: row.Index, Table: table, MAC: mac, VRF: vrf}
	}
	if len(result) != len(selected) {
		return nil, fmt.Errorf("the Linux physical interface coverage is incomplete")
	}
	return result, nil
}

// ASICRIFDump owns validated SDK rows without exposing a generic dump in inventory.
type ASICRIFDump struct{ rows map[uint32]asicRIF }

// ParseASICRIFDump rejects error-shaped successful exits before further remote
// reads. The stock command can exit zero with an uninitialized router module.
func ParseASICRIFDump(raw []byte) (*ASICRIFDump, error) {
	rows, err := parseASICRIFs(raw)
	if err != nil {
		return nil, err
	}
	return &ASICRIFDump{rows: rows}, nil
}

type asicRIF struct {
	ID, RouterID, LogPort, MTU uint32
	Type, MAC                  string
	IPv4, IPv6                 bool
	IPv4Known, IPv6Known       bool
}

var rifHeader = []string{"RIF", "Router ID", "TYPE", "L2 SWID", "L2 VLAN", "LOG PORT", "BRIDGE", "EGRESS_MODE", "L2 MAC", "L2 MTU", "L3 TTL", "QOS", "ipv4 UC state", "ipv6 UC state", "ipv4 MC state", "ipv6 MC state", "LB Enabled"}

func parseASICRIFs(raw []byte) (map[uint32]asicRIF, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, fmt.Errorf("SDK RIF output is absent or exceeds its limit")
	}
	result := map[uint32]asicRIF{}
	headerSeen := false
	lastSeparator := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if separator(line, '-') {
			lastSeparator = true
			continue
		}
		lastSeparator = false
		if !strings.HasPrefix(line, "|") {
			// Allow the exact installed vendor disclaimer and bounded SDK startup log;
			// an error, traceback or other diagnostic can never become an empty pass.
			if headerSeen || !rifPreamble(line) {
				return nil, fmt.Errorf("SDK RIF command reported an error or unsupported banner")
			}
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 19 || cells[0] != "" || cells[18] != "" {
			return nil, fmt.Errorf("SDK RIF table schema is unsupported")
		}
		cells = cells[1:18]
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "RIF" {
			if headerSeen || !reflect.DeepEqual(cells, rifHeader) {
				return nil, fmt.Errorf("SDK RIF header is duplicated or unsupported")
			}
			headerSeen = true
			continue
		}
		if !headerSeen {
			return nil, fmt.Errorf("SDK RIF table lacks its header")
		}
		row := asicRIF{Type: cells[2]}
		var err error
		for _, pair := range []struct {
			index int
			dst   *uint32
			base  int
		}{{0, &row.ID, 10}, {1, &row.RouterID, 10}, {5, &row.LogPort, 0}, {9, &row.MTU, 10}} {
			*pair.dst, err = asicUint(cells[pair.index], pair.base)
			if err != nil {
				return nil, fmt.Errorf("SDK RIF numeric field is malformed")
			}
		}
		row.MAC, err = asicMAC(cells[8])
		if err != nil {
			return nil, err
		}
		for _, index := range []int{3, 4, 6, 10} {
			if _, err := asicUint(cells[index], 10); err != nil {
				return nil, fmt.Errorf("SDK RIF numeric field is malformed")
			}
		}
		for _, index := range []int{12, 13, 14, 15, 16} {
			if cells[index] != "ENABLE" && cells[index] != "DISABLE" && cells[index] != "UNKOWN" {
				return nil, fmt.Errorf("SDK RIF state is unsupported")
			}
		}
		if _, exists := result[row.ID]; exists {
			return nil, fmt.Errorf("SDK RIF identity is duplicated")
		}
		row.IPv4Known = cells[12] != "UNKOWN"
		row.IPv6Known = cells[13] != "UNKOWN"
		row.IPv4 = cells[12] == "ENABLE"
		row.IPv6 = cells[13] == "ENABLE"
		result[row.ID] = row
	}
	if !headerSeen || len(result) == 0 || !lastSeparator {
		return nil, fmt.Errorf("SDK RIF table is empty or incomplete")
	}
	return result, nil
}

var sdkStartupLog = regexp.MustCompile(`^[A-Z][a-z]{2} [ 0-9][0-9] [0-9:]{8} DEBUG   LOG: Initializing SX log with STDOUT as output file\.$`)

func rifPreamble(line string) bool {
	switch line {
	case "DISCLAIMER", "==========", "The following code is an EXAMPLE whose sole purpose is to demonstrate various capabilities of the SDK.", "This example represents a common configuration scenario and is in no way a complete guide to the demonstrated feature.", "For complete documentation please refer to the SDK API Guide.", "There is no guarantee that this example will run successfully on all hardware and software configurations.":
		return true
	}
	return sdkStartupLog.MatchString(line)
}

type asicIdentity struct {
	tables     ASICBackendTables
	interfaces map[string]asicInterface
	vrfs       map[uint32]uint32
	linux      map[string]linuxASICPort
	config     asicConfiguration
}

// ValidateASICIdentity checks software coverage before the SDK diagnostic.
func ValidateASICIdentity(in ASICIdentityInputs, ports []string) error {
	_, err := parseASICIdentity(in, ports)
	return err
}

func parseASICIdentity(in ASICIdentityInputs, ports []string) (asicIdentity, error) {
	selected := map[string]bool{}
	for _, name := range ports {
		if name == "" || selected[name] {
			return asicIdentity{}, fmt.Errorf("ASIC selected port identity is invalid")
		}
		selected[name] = true
	}
	if len(selected) == 0 {
		return asicIdentity{}, fmt.Errorf("ASIC selected port coverage is empty")
	}
	tables, err := ParseASICBackendTables(in.Root)
	if err != nil {
		return asicIdentity{}, err
	}
	if backendID(in.Interfaces) != tables.Interfaces || backendID(in.VRFs) != tables.VRFs {
		return asicIdentity{}, fmt.Errorf("switchd table header identity disagrees with root selection")
	}
	// On this qualified build, nv config diff exits zero with no stdout when
	// there are no pending changes. A non-nil empty slice records that successful
	// command; nil still means no observation. Nonempty output must be an empty
	// JSON object, never a nonempty diff, whitespace or an error-shaped response.
	if in.Pending == nil {
		return asicIdentity{}, fmt.Errorf("NVUE pending changes are present or unmeasured")
	}
	if len(in.Pending) > 0 {
		var pending map[string]json.RawMessage
		if asicJSON(in.Pending, &pending) != nil || pending == nil || len(pending) != 0 {
			return asicIdentity{}, fmt.Errorf("NVUE pending changes are present or unmeasured")
		}
	}
	config, err := parseASICConfiguration(in.Config)
	if err != nil {
		return asicIdentity{}, err
	}
	interfaces, err := parseASICInterfaces(in.Interfaces, selected)
	if err != nil {
		return asicIdentity{}, err
	}
	vrfs, err := parseASICVRFs(in.VRFs)
	if err != nil {
		return asicIdentity{}, err
	}
	linux, err := parseASICLinux(in.Linux, selected)
	if err != nil {
		return asicIdentity{}, err
	}
	for name, physical := range interfaces {
		host := linux[name]
		if physical.MAC != host.MAC || physical.Index != host.Index || physical.Table != host.Table {
			return asicIdentity{}, fmt.Errorf("the Linux and backend physical interface identities disagree")
		}
	}
	return asicIdentity{tables: tables, interfaces: interfaces, vrfs: vrfs, linux: linux, config: config}, nil
}

// ParseASICObservation joins complete before/after software identity snapshots
// with the direct ASIC RIF dump. It never derives VRIDs or logical-port IDs.
func ParseASICObservation(in ASICInputs, ports []string) (*ASICObservation, error) {
	identity, err := parseASICIdentity(in.Before, ports)
	if err != nil {
		return nil, err
	}
	after, err := parseASICIdentity(in.After, ports)
	if err != nil {
		return nil, err
	}
	if identity.tables != after.tables || !maps.Equal(identity.interfaces, after.interfaces) || !maps.Equal(identity.vrfs, after.vrfs) || !maps.Equal(identity.linux, after.linux) || !reflect.DeepEqual(identity.config, after.config) {
		return nil, fmt.Errorf("physical, VRF or configuration identity changed during ASIC collection")
	}
	interfaces, vrfs, linux := identity.interfaces, identity.vrfs, identity.linux
	if in.RIFs == nil {
		return nil, fmt.Errorf("direct SDK RIF read is absent")
	}
	rifs := in.RIFs.rows
	out := &ASICObservation{Ports: make([]ASICPort, 0, len(ports))}
	seenRIFs := map[uint32]bool{}
	seenPorts := map[uint32]bool{}
	for _, name := range ports {
		physical := interfaces[name]
		host := linux[name]
		rif, ok := rifs[physical.RIF]
		if !ok || !rif.IPv4Known || !rif.IPv6Known || rif.Type != "VPORT" || rif.MAC != physical.MAC || rif.LogPort != physical.VPort || physical.MAC != host.MAC || physical.Index != host.Index || physical.Table != host.Table || seenRIFs[rif.ID] || seenPorts[rif.LogPort] {
			return nil, fmt.Errorf("selected SDK RIF identity is missing, unsupported or ambiguous")
		}
		seenRIFs[rif.ID] = true
		seenPorts[rif.LogPort] = true
		table, ok := vrfs[rif.RouterID]
		if !ok {
			return nil, fmt.Errorf("actual SDK router has no backend Linux table mapping")
		}
		// A software/ASIC VRF disagreement is retained as an observed mismatch,
		// rather than discarding the direct read or replacing it with Linux intent.
		out.Ports = append(out.Ports, ASICPort{Name: name, RIF: rif.ID, RouterID: rif.RouterID, RouterTable: table, LinuxVRF: host.VRF, LinuxTable: host.Table, MAC: rif.MAC, MTU: rif.MTU, IPv4Enabled: rif.IPv4, IPv6Enabled: rif.IPv6})
	}
	return out, nil
}

func backendCount(raw []byte) int {
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 2 {
		return -1
	}
	_, count, ok := strings.Cut(lines[1], " Count: ")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return -1
	}
	return n
}

// Reject duplicate object keys before decoding typed identity/config values.
// encoding/json alone silently accepts the last duplicate value.
func asicJSON(raw []byte, value any) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return fmt.Errorf("JSON source is absent or exceeds its limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("JSON source exceeds nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate JSON identity field")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("invalid JSON array")
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("JSON source has trailing content")
	}
	valueDecoder := json.NewDecoder(bytes.NewReader(raw))
	valueDecoder.UseNumber()
	return valueDecoder.Decode(value)
}

func backendID(raw []byte) uint32 {
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 2 {
		return 0
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 || fields[0] != "TableId:" {
		return 0
	}
	id, err := asicUint(fields[1], 10)
	if err != nil {
		return 0
	}
	return id
}

// The qualified NVUE config-show command emits an ordered document, not the
// object returned by individual nv show queries. Keep the complete header and
// set tree in the fence; no header property is assumed volatile or discarded.
type asicConfiguration struct {
	header   map[string]any
	settings map[string]any
}

func parseASICConfiguration(raw []byte) (asicConfiguration, error) {
	var document []map[string]json.RawMessage
	if asicJSON(raw, &document) != nil || len(document) != 2 || len(document[0]) != 1 || len(document[1]) != 1 {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration requires an ordered header/set document")
	}
	var header map[string]any
	if asicJSON(document[0]["header"], &header) != nil || len(header) != 4 {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration header is missing or unsupported")
	}
	for _, field := range []string{"model", "nvue-api-version", "version"} {
		value, ok := header[field].(string)
		if !ok || strings.TrimSpace(value) == "" || len(value) > 256 {
			return asicConfiguration{}, fmt.Errorf("NVUE applied configuration header field is malformed")
		}
	}
	revision, ok := header["rev-id"].(json.Number)
	if !ok {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration revision is missing or malformed")
	}
	number, err := revision.Float64()
	if err != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration revision is unsupported")
	}
	var settings map[string]any
	if asicJSON(document[1]["set"], &settings) != nil || len(settings) == 0 {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration set tree is missing or empty")
	}
	knownNonempty := false
	for name, value := range settings {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return asicConfiguration{}, fmt.Errorf("NVUE applied configuration set domain is malformed")
		}
		switch name {
		case "interface", "router", "system", "vrf", "bridge":
			knownNonempty = knownNonempty || len(object) > 0
		}
	}
	if !knownNonempty {
		return asicConfiguration{}, fmt.Errorf("NVUE applied configuration lacks a nonempty configuration domain")
	}
	return asicConfiguration{header: header, settings: settings}, nil
}
