// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"strconv"
	"strings"
)

// The host PCI denominator is independent of the GPU runtime's device view.
// Expected NIC addresses are passed as positional arguments, never shell code.
const gpuHostProbeCmd = `root=$1
shift
[ -d "$root" ] && [ -r "$root" ] && [ -x "$root" ] || exit 1
count=0
for d in "$root"/*; do
  [ -e "$d" ] || continue
  vendor=$(cat "$d/vendor") || exit 1
  [ "$vendor" = 0x10de ] || continue
  class=$(cat "$d/class") || exit 1
  case "$class" in 0x030000|0x030200) ;; *) continue ;; esac
  [ ! -e "$d/physfn" ] || exit 1
  printf 'GPU\t%s\n' "${d##*/}"
  count=$((count + 1))
done
printf 'HOSTDONE\t%s\n' "$count"
for pci in "$@"; do
  d="$root/$pci/infiniband"
  if [ -d "$d" ] && [ -r "$d" ] && [ -x "$d" ]; then
    for r in "$d"/*; do
      [ -e "$r" ] || continue
      printf 'NIC\t%s\t%s\n' "$pci" "${r##*/}"
    done
  fi
done
echo DONE`

// Each section has its own exit status: product discovery may succeed even
// when NVML cannot provide a topology matrix. No host files or pod environment
// are copied into the result. Runtime-injected nvidia-smi takes precedence.
const gpuObservationCmd = `smi=''
if [ "${1:-local}" = host ]; then
  if [ -x /host/usr/bin/nvidia-smi ]; then
    smi=/host/usr/bin/nvidia-smi
    export LD_LIBRARY_PATH=/host/usr/lib/x86_64-linux-gnu:/host/usr/lib/aarch64-linux-gnu:${LD_LIBRARY_PATH:-}
  fi
else
  smi=$(command -v nvidia-smi || true)
fi
if [ -z "$smi" ]; then echo '---UNAVAILABLE---'; exit 0; fi
echo '---PRODUCT---'
"$smi" -q
printf '\n---PRODUCT-STATUS---\n%s\n' "$?"
echo '---QUERY---'
"$smi" --query-gpu=index,pci.bus_id --format=csv,noheader
printf '\n---QUERY-STATUS---\n%s\n' "$?"
echo '---TOPO---'
"$smi" topo -mp
printf '\n---TOPO-STATUS---\n%s\n' "$?"
echo '---DONE---'`

type gpuHostObservation struct {
	gpus map[string]bool
	rdma map[string]string
}

func parseGPUHostObservation(output string) (gpuHostObservation, error) {
	result := gpuHostObservation{gpus: map[string]bool{}, rdma: map[string]string{}}
	aliasPCI := map[string]string{}
	hostDone, done := false, false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || done {
			return result, fmt.Errorf("invalid host GPU enumeration")
		}
		switch fields[0] {
		case "GPU":
			if len(fields) != 2 || hostDone {
				return result, fmt.Errorf("invalid host GPU record")
			}
			pci := normalizePCI(fields[1])
			if !fabricPCIRE.MatchString(pci) || result.gpus[pci] {
				return result, fmt.Errorf("invalid or duplicate host GPU PCI")
			}
			result.gpus[pci] = true
		case "HOSTDONE":
			if len(fields) != 2 || hostDone || fields[1] != strconv.Itoa(len(result.gpus)) {
				return result, fmt.Errorf("incomplete host GPU enumeration")
			}
			hostDone = true
		case "NIC":
			if len(fields) != 3 || !hostDone || !fabricPCIRE.MatchString(normalizePCI(fields[1])) || !fabricRDMARE.MatchString(fields[2]) {
				return result, fmt.Errorf("invalid host NIC identity")
			}
			pci := normalizePCI(fields[1])
			if _, exists := result.rdma[pci]; exists {
				// Multiple RDMA aliases for one PCI are not an unambiguous join.
				result.rdma[pci] = ""
			} else {
				result.rdma[pci] = fields[2]
			}
			if previous, exists := aliasPCI[fields[2]]; exists && previous != pci {
				result.rdma[previous], result.rdma[pci] = "", ""
			}
			aliasPCI[fields[2]] = pci
		case "DONE":
			if len(fields) != 1 || !hostDone {
				return result, fmt.Errorf("incomplete host observation")
			}
			done = true
		default:
			return result, fmt.Errorf("unexpected host observation record")
		}
	}
	if !done {
		return result, fmt.Errorf("host observation did not complete")
	}
	return result, nil
}

// parseGPUObservation validates independent physical-device coverage before
// accepting a matrix. The older permissive parser remains useful for captured
// legacy fixtures, but cannot establish completeness for live observations.
func parseGPUObservation(output string, host gpuHostObservation) (string, topoData, error) {
	data := topoData{gpuPCI: map[int]string{}, matrix: map[string]map[int]proximity{}}
	markers := []string{"PRODUCT", "PRODUCT-STATUS", "QUERY", "QUERY-STATUS", "TOPO", "TOPO-STATUS", "DONE"}
	sections := map[string]string{}
	current := ""
	next := 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if next < len(markers) && line == "---"+markers[next]+"---" {
			current = markers[next]
			next++
		} else if current == "" || current == "DONE" || strings.HasPrefix(line, "---") {
			return "", data, fmt.Errorf("GPU command unavailable or incomplete")
		} else {
			sections[current] += line + "\n"
		}
	}
	product := ""
	if strings.TrimSpace(sections["PRODUCT-STATUS"]) == "0" {
		product = parseGPUProductFromNvidiaSmi(sections["PRODUCT"])
	}
	if next != len(markers) || strings.TrimSpace(sections["QUERY-STATUS"]) != "0" || strings.TrimSpace(sections["TOPO-STATUS"]) != "0" {
		return product, data, fmt.Errorf("GPU query/topology command failed or did not complete (query exit %q, topology exit %q)",
			strings.TrimSpace(sections["QUERY-STATUS"]), strings.TrimSpace(sections["TOPO-STATUS"]))
	}
	seenPCI := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(sections["QUERY"]), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return product, data, fmt.Errorf("invalid GPU query record")
		}
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		pci := normalizePCI(fields[1])
		_, duplicate := data.gpuPCI[index]
		if err != nil || index < 0 || duplicate || seenPCI[pci] || !host.gpus[pci] {
			return product, data, fmt.Errorf("GPU query is not a unique mapping of host physical GPUs")
		}
		data.gpuPCI[index], seenPCI[pci] = pci, true
	}
	if len(host.gpus) == 0 || len(seenPCI) != len(host.gpus) {
		return product, data, fmt.Errorf("GPU query does not cover all host physical GPUs")
	}
	if err := validateGPUMatrix(sections["TOPO"], data.gpuPCI); err != nil {
		return product, data, err
	}
	parseTopoMatrix(sections["TOPO"], data.matrix)
	return product, data, nil
}

// Require one GPU header/row per queried index and a unique NIC legend. NIC
// proximity completeness is checked per expected PCI during tuple attachment.
func validateGPUMatrix(block string, gpuPCI map[int]string) error {
	header, rows := map[int]bool{}, map[int]bool{}
	nicRows, legend, aliases := map[string]bool{}, map[string]bool{}, map[string]bool{}
	headerFound, inLegend := false, false
	for _, line := range strings.Split(stripANSI(block), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !headerFound && fields[0] == "GPU0" {
			for _, field := range fields {
				index, err := strconv.Atoi(strings.TrimPrefix(field, "GPU"))
				if !strings.HasPrefix(field, "GPU") || err != nil {
					continue
				}
				if _, ok := gpuPCI[index]; !ok || header[index] {
					return fmt.Errorf("unexpected or duplicate GPU matrix column")
				}
				header[index] = true
			}
			headerFound = true
			continue
		}
		if strings.TrimSpace(line) == "NIC Legend:" {
			inLegend = true
			continue
		}
		if !headerFound {
			continue
		}
		if strings.HasPrefix(fields[0], "GPU") && !inLegend {
			index, err := strconv.Atoi(strings.TrimPrefix(fields[0], "GPU"))
			if err != nil || !header[index] || rows[index] || len(fields) < len(header)+1 {
				return fmt.Errorf("invalid or incomplete GPU matrix row")
			}
			rows[index] = true
		}
		if !strings.HasPrefix(fields[0], "NIC") {
			continue
		}
		if !inLegend {
			if nicRows[fields[0]] {
				return fmt.Errorf("duplicate NIC matrix row")
			}
			nicRows[fields[0]] = true
		} else {
			label := strings.TrimSuffix(fields[0], ":")
			if len(fields) != 2 || !nicRows[label] || legend[label] || aliases[fields[1]] {
				return fmt.Errorf("ambiguous NIC matrix legend")
			}
			legend[label], aliases[fields[1]] = true, true
		}
	}
	if len(header) != len(gpuPCI) || len(rows) != len(gpuPCI) || len(legend) != len(nicRows) || !inLegend {
		return fmt.Errorf("GPU matrix/legend coverage is incomplete")
	}
	return nil
}
