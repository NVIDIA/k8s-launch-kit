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

package networkoperatorplugin

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNotInstalled reports that no existing NIC Configuration Daemon was found
// for a read-only discovery request.
var ErrNotInstalled = discovery.ErrNotInstalled

// ReadOnlyOption configures read-only discovery.
type ReadOnlyOption = discovery.ReadOnlyOption

// WithReadOnlyNetworkOperatorNamespace restricts read-only daemon lookup to
// the configured Network Operator namespace.
var WithReadOnlyNetworkOperatorNamespace = discovery.WithReadOnlyNetworkOperatorNamespace

// DiscoverReadOnly inspects an existing NIC Configuration Daemon and returns
// the discovered topology without creating, patching, or deleting resources.
// It returns ErrNotInstalled when the daemon is not already running.
func DiscoverReadOnly(ctx context.Context, kubeClient client.Client, opts ...ReadOnlyOption) (*config.LaunchKitConfig, error) {
	return discovery.DiscoverReadOnly(ctx, kubeClient, opts...)
}
