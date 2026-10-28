// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const fabricProbeTimeout = 10 * time.Second
const fabricProbeConcurrency = 4

var fabricPCIRE = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
var fabricRDMARE = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

type fabricPort struct{ pci, rdma, port string }
type fabricObservation struct {
	fabric string
	netdev bool
}
type nodeFabricObservation struct {
	ports map[fabricPort]fabricObservation
	err   error
}

// discoverFabric owns group mutation after bounded node-local collection. The
// parser proves coverage against inventory PCI expectations and an independently
// completed sysfs port manifest, never merely against successful link_layer reads.
func discoverFabric(ctx context.Context, restConfig *rest.Config, namespace string, groups []config.ClusterConfig,
	devices []nicop.NicDevice, members groupPFMembers, pods []corev1.Pod, exec readOnlyPodExec) error {
	return discoverFabricWithTimeout(ctx, restConfig, namespace, groups, devices, members, pods, exec, fabricProbeTimeout)
}

func discoverFabricWithTimeout(ctx context.Context, restConfig *rest.Config, namespace string, groups []config.ClusterConfig,
	devices []nicop.NicDevice, members groupPFMembers, pods []corev1.Pod, exec readOnlyPodExec, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if exec == nil {
		exec = execInPod
	}
	identities := buildNodePFIdentities(groups, devices, members)
	byNode := map[string]map[string]bool{}
	for _, identity := range identities {
		if byNode[identity.node] == nil {
			byNode[identity.node] = map[string]bool{}
		}
		byNode[identity.node][identity.pci] = true
	}
	nodes := make([]string, 0, len(byNode))
	for node := range byNode {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	results := make([]nodeFabricObservation, len(nodes))
	jobs := make(chan int)
	var denied atomic.Bool
	var workers sync.WaitGroup
	for range min(fabricProbeConcurrency, len(nodes)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				if denied.Load() {
					results[i].err = fmt.Errorf("fabric exec permission unavailable")
					continue
				}
				node := nodes[i]
				pod := findDaemonPod([]string{node}, pods)
				if pod == nil || podContainerName(pod) == "" {
					results[i].err = fmt.Errorf("no usable daemon container")
					continue
				}
				pcis := make([]string, 0, len(byNode[node]))
				for pci := range byNode[node] {
					pcis = append(pcis, pci)
				}
				slices.Sort(pcis)
				valid := true
				for _, pci := range pcis {
					if !fabricPCIRE.MatchString(pci) {
						valid = false
					}
				}
				if !valid {
					results[i].err = fmt.Errorf("invalid inventory PCI address")
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				command := append([]string{"/bin/sh", "-c", fabricProbeCmd, "l8k-fabric"}, pcis...)
				output, err := exec(probeCtx, restConfig, namespace, pod.Name, podContainerName(pod), command)
				if err == nil && probeCtx.Err() != nil {
					err = probeCtx.Err()
				}
				cancel()
				if err != nil {
					if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
						denied.Store(true)
					}
					results[i].err = err
				} else {
					results[i] = parseFabricObservation(output, pcis)
				}
			}
		}()
	}
dispatch:
	for i := range nodes {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- i:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	observations := map[string]nodeFabricObservation{}
	for i, node := range nodes {
		observations[node] = results[i]
		if results[i].err != nil {
			log.Log.V(1).Info("Fabric observation unavailable", "node", node, "error", results[i].err.Error())
		}
	}
	applyFabricObservations(ctx, groups, identities, observations)
	return nil
}

// applyFabricObservations has sole ownership of group fabric mutation. Expected
// identities are independent of successful probe results.
func applyFabricObservations(ctx context.Context, groups []config.ClusterConfig, identities []nodePFIdentity, observations map[string]nodeFabricObservation) {
	for i := range groups {
		groups[i].LinkType = ""
		fabric := ""
		complete := true
		count := 0
		var reasons []string
		var noNetdev []string
		for _, identity := range identities {
			if identity.groupIndex != i {
				continue
			}
			count++
			observation := observations[identity.node]
			reason := identity.missing
			if observation.err != nil {
				reason = "node probe unavailable"
			}
			found := false
			for port, value := range observation.ports {
				if port.pci != identity.pci {
					continue
				}
				found = true
				if fabric == "" {
					fabric = value.fabric
				} else if fabric != value.fabric {
					reason = "fabric disagreement"
				}
				if value.fabric == "InfiniBand" && !value.netdev {
					noNetdev = append(noNetdev, identity.node+"/"+identity.pci)
				}
			}
			if !found {
				reason = "missing PCI port observations"
			}
			if reason != "" {
				complete = false
				reasons = append(reasons, identity.node+"/"+identity.pci+": "+reason)
			}
		}
		if complete && count > 0 && fabric != "" {
			groups[i].LinkType = fabric
		} else {
			slices.Sort(reasons)
			reasons = slices.Compact(reasons)
			ui.FromContext(ctx).Warning("Fabric remains unknown for group %q: %s; use --fabric explicitly after checking the selected NICs", groups[i].Identifier, boundedFabricReasons(reasons))
		}
		slices.Sort(noNetdev)
		noNetdev = slices.Compact(noNetdev)
		if len(noNetdev) > 0 {
			ui.FromContext(ctx).Warning("InfiniBand PFs have no host netdev at %s; IPoIB requires host provisioning (check ib_ipoib). This does not establish RDMA shared-plugin device readiness", boundedFabricReasons(noNetdev))
		}
	}
}

func boundedFabricReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "no complete selected worker/PF observations"
	}
	n := min(5, len(reasons))
	result := strings.Join(reasons[:n], "; ")
	if len(reasons) > n {
		result += fmt.Sprintf("; and %d more", len(reasons)-n)
	}
	return truncateForLog(result, 700)
}

func parseFabricObservation(output string, expected []string) nodeFabricObservation {
	fail := func(reason string) nodeFabricObservation {
		return nodeFabricObservation{err: fmt.Errorf("incomplete fabric observation: %s", reason)}
	}
	if len(output) > 1024*1024 {
		return fail("output too large")
	}
	expectedSet := map[string]bool{}
	for _, pci := range expected {
		if !fabricPCIRE.MatchString(pci) || expectedSet[pci] {
			return fail("invalid expected PCI set")
		}
		expectedSet[pci] = true
	}
	started, ended := map[string]bool{}, map[string]bool{}
	manifest := map[fabricPort]bool{}
	result := nodeFabricObservation{ports: map[fabricPort]fabricObservation{}}
	counts := map[string]int{}
	done := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if done {
			return fail("records after completion")
		}
		if len(fields) < 2 {
			return fail("malformed record")
		}
		pci := fields[1]
		switch fields[0] {
		case "BEGIN":
			if len(fields) != 2 || !expectedSet[pci] || started[pci] {
				return fail("unexpected/duplicate PCI start")
			}
			started[pci] = true
		case "PORT":
			if len(fields) != 4 || !started[pci] || ended[pci] || !fabricRDMARE.MatchString(fields[2]) {
				return fail("invalid port manifest")
			}
			port, err := strconv.Atoi(fields[3])
			if err != nil || port < 1 {
				return fail("invalid port number")
			}
			key := fabricPort{pci, fields[2], fields[3]}
			if manifest[key] {
				return fail("duplicate manifest port")
			}
			manifest[key] = true
			counts[pci]++
		case "END":
			if len(fields) != 3 || !started[pci] || ended[pci] {
				return fail("unexpected PCI end")
			}
			count, err := strconv.Atoi(fields[2])
			if err != nil || count == 0 || count != counts[pci] {
				return fail("incomplete enumeration")
			}
			ended[pci] = true
		case "RESULT":
			if len(fields) != 6 || len(ended) != len(expectedSet) {
				return fail("result before complete manifest")
			}
			key := fabricPort{pci, fields[2], fields[3]}
			if !manifest[key] {
				return fail("unexpected result")
			}
			if _, exists := result.ports[key]; exists {
				return fail("duplicate result")
			}
			fabric := normalizeLinkLayer(fields[4])
			if fabric == "" || (fields[5] != "yes" && fields[5] != "no") {
				return fail("unreadable link layer or netdev state")
			}
			result.ports[key] = fabricObservation{fabric: fabric, netdev: fields[5] == "yes"}
		case "DONE":
			if len(fields) != 3 {
				return fail("malformed completion")
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil || n != len(expectedSet) {
				return fail("incomplete PCI count")
			}
			ports, err := strconv.Atoi(fields[2])
			if err != nil || ports != len(manifest) || len(result.ports) != len(manifest) || len(ended) != len(expectedSet) {
				return fail("incomplete result count")
			}
			done = true
		default:
			return fail("unknown or failed record")
		}
	}
	if scanner.Err() != nil || !done {
		return fail("missing completion")
	}
	return result
}

// PCI arguments are validated by Go and passed as positional arguments. Names
// come only from the PCI device's own sysfs subtree, never saved group aliases.
// All port manifests complete before any link_layer reads; every failure exits
// without DONE, so partial shell output cannot prove unanimous fabric.
const fabricProbeCmd = `set -f
manifest=''
port_count=0
pci_count=$#
for pci do
 printf 'BEGIN\t%s\n' "$pci"
 base=/sys
 path="$base/bus/pci/devices/$pci"
 if [ ! -d "$path/infiniband" ]; then base=/host/sys; path="$base/bus/pci/devices/$pci"; fi
 [ -r "$path/infiniband" ] && [ -x "$path/infiniband" ] || exit 1
 rdmas=$(ls -1A "$path/infiniband") || exit 1
 [ -n "$rdmas" ] || exit 1
 count=0
 for rdma in $rdmas; do
  case "$rdma" in *[!a-zA-Z0-9_.-]*|'') exit 1;; esac
  ports="$path/infiniband/$rdma/ports"
  [ -r "$ports" ] && [ -x "$ports" ] || exit 1
  numbers=$(ls -1A "$ports") || exit 1
  [ -n "$numbers" ] || exit 1
  for port in $numbers; do
   case "$port" in *[!0-9]*|'') exit 1;; esac
   [ "$port" -gt 0 ] || exit 1
   printf 'PORT\t%s\t%s\t%s\n' "$pci" "$rdma" "$port"
   manifest="$manifest $pci,$rdma,$port,$base"
   count=$((count+1)); port_count=$((port_count+1))
  done
 done
 printf 'END\t%s\t%s\n' "$pci" "$count"
done
for entry in $manifest; do
 old_ifs=$IFS; IFS=,; set -- $entry; IFS=$old_ifs
 pci=$1; rdma=$2; port=$3; base=$4
 path="$base/bus/pci/devices/$pci"
 fabric=$(cat "$path/infiniband/$rdma/ports/$port/link_layer") || exit 1
 netdev=no
 if [ -d "$path/net" ]; then
  netdevs=$(ls -1A "$path/net") || exit 1
  [ -z "$netdevs" ] || netdev=yes
 else
  children=$(ls -1A "$path") || exit 1
  for child in $children; do [ "$child" != net ] || exit 1; done
 fi
 printf 'RESULT\t%s\t%s\t%s\t%s\t%s\n' "$pci" "$rdma" "$port" "$fabric" "$netdev"
done
printf 'DONE\t%s\t%s\n' "$pci_count" "$port_count"`
