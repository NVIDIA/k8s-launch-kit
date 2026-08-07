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

package cmd

import hosttarget "github.com/nvidia/k8s-launch-kit/pkg/target/host"

// defaultUserConfigPath remains the CLI compatibility constant used by tests
// and help. Host path resolution is implemented by pkg/target/host.
const defaultUserConfigPath = "./cluster-config.yaml"

func currentUserConfigInput(configRoot string) hosttarget.UserConfigInput {
	return hosttarget.UserConfigInput{
		Explicit:        userConfig,
		DeploymentFiles: deploymentFiles,
		ConfigDir:       configRoot,
	}
}

func userConfigPathFor(configRoot string) (string, error) {
	return hosttarget.UserConfigPathFor(currentUserConfigInput(configRoot))
}

func userConfigPathBeforeDefaults() string {
	return hosttarget.UserConfigPathBeforeDefaults(currentUserConfigInput(configDir))
}

func defaultConfigPathFor(configRoot string) (string, error) {
	return hosttarget.DefaultConfigPathFor(configRoot)
}

func userConfigPathForGenerate(configRoot string) string {
	return hosttarget.UserConfigPathForGenerate(currentUserConfigInput(configRoot))
}
