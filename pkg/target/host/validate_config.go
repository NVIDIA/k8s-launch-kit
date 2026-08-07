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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func applyValidationOverrides(request ValidateRequest, validationCfg *config.ValidationConfig) error {
	if validationCfg == nil {
		return fmt.Errorf("validation config must not be nil")
	}
	if request.Connectivity.Set {
		value := request.Connectivity.Value
		validationCfg.Connectivity = &value
	}
	if request.Mode.Set {
		validationCfg.Mode = strings.TrimSpace(request.Mode.Value)
	}
	if request.Checks.Set {
		validationCfg.Checks = config.NormalizeValidationChecks(request.Checks.Value)
	}
	if validationCfg.RDMA == nil {
		validationCfg.RDMA = &config.ValidationRDMAConfig{}
	}
	if request.RDMAPIterations.Set {
		validationCfg.RDMA.RPingIterations = request.RDMAPIterations.Value
	}
	if request.RDMAIBWriteSize.Set {
		validationCfg.RDMA.IBWriteSize = request.RDMAIBWriteSize.Value
	}
	if request.RDMAMinBandwidth.Set {
		value := request.RDMAMinBandwidth.Value
		validationCfg.RDMA.IBWriteMinBandwidthGbps = &value
	}
	normalized := config.NormalizeValidationConfig(validationCfg)
	*validationCfg = *normalized
	return config.ValidateValidationConfig(validationCfg)
}
