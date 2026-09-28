// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func hasDeploymentValidationInputsBundle(artifacts *bundle.Bundle) bool {
	if artifacts == nil {
		return false
	}
	for _, file := range artifacts.Files() {
		if !bundle.IsExampleFilename(file.Name) {
			return true
		}
	}
	return false
}

func validateConnectivityDaemonSets(
	objects []*unstructured.Unstructured,
	refs []connectivity.DaemonSetRef,
	checks []connectivity.Check,
) error {
	if len(objects) == 0 {
		return fmt.Errorf("no example test DaemonSet found under --deployment-files")
	}
	if len(objects) != len(refs) {
		return fmt.Errorf("loaded %d connectivity DaemonSets but resolved %d references", len(objects), len(refs))
	}

	requireRouteHelper := len(checks) > 0
	requireRDMA := connectivityChecksContain(checks, connectivity.CheckRPing) ||
		connectivityChecksContain(checks, connectivity.CheckIBWriteBW) ||
		connectivityChecksContain(checks, connectivity.CheckGPUDirectDMABuf)
	seen := map[string]bool{}
	for index, object := range objects {
		ref := refs[index]
		if object.GetAPIVersion() != "apps/v1" {
			return fmt.Errorf("example test DaemonSet %s from %s must use apiVersion apps/v1",
				ref.Name, ref.SourceFile)
		}
		if strings.TrimSpace(ref.Namespace) == "" {
			return fmt.Errorf("example test DaemonSet %s from %s must set metadata.namespace",
				ref.Name, ref.SourceFile)
		}
		key := ref.Namespace + "/" + ref.Name
		if seen[key] {
			return fmt.Errorf("example test DaemonSet %s is declared more than once", key)
		}
		seen[key] = true
		if requireRouteHelper && ref.ICMPContainer == "" {
			return fmt.Errorf("example test DaemonSet %s from %s must declare the netshoot ICMP/route helper container",
				key, ref.SourceFile)
		}
		if requireRDMA && ref.RDMAContainer == "" {
			return fmt.Errorf("example test DaemonSet %s from %s must declare an RDMA tools container",
				key, ref.SourceFile)
		}
	}
	return nil
}
