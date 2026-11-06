// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package sentry renders the bounded BlueField-4 Sentry profile offline.
package sentry

// ChartLock identifies exact local archive bytes and their upstream coordinates.
// Archives are explicit inputs; this package never downloads them.
type ChartLock struct {
	Name, Version, Repository, Filename, SHA256 string
}

// ImageLock identifies immutable image bytes without inspecting a registry.
type ImageLock struct {
	Name, Repository, Tag, Digest string
}

func (i ImageLock) TagDigest() string { return i.Tag + "@" + i.Digest }
func (i ImageLock) Reference() string { return i.Repository + ":" + i.TagDigest() }

// Catalog returns isolated release locks for the reviewed staging profile. These
// coordinates establish source identity, not release or hardware qualification.
func Catalog() []ChartLock {
	return []ChartLock{
		{"doca-tower", "0.2.16", "oci://nvcr.io/nvstaging/doca", "doca-tower-0.2.16.tgz", "f88885808c8b3a84a2c0f1871e5c3341ded3000e77342e34a289717ae45a3628"},
		{"doca-sentry-policy-controller", "0.2.5", "https://helm.ngc.nvidia.com/nvstaging/doca", "doca-sentry-policy-controller-0.2.5.tgz", "553dd95f207fdc1194803d2b041721c93393c8b91a592371c4203d3f4bc4a548"},
		{"doca-argus", "1.5.1-ai-gateway.8", "https://helm.ngc.nvidia.com/nvstaging/doca", "doca-argus-1.5.1-ai-gateway.8.tgz", "de0d228f81167d641bb25f12415544c5eaff91cfd673f47e62ae4fcbbbdfd79f"},
		{"doca-sentry-policy-server", "0.2.5", "https://helm.ngc.nvidia.com/nvstaging/doca", "doca-sentry-policy-server-0.2.5.tgz", "ffad2db31039cc4c4e80c718cac1974a9f88acdd88af7376943aacb6956807ea"},
	}
}

// Images returns the immutable image identities used by the profile templates.
func Images() []ImageLock {
	return []ImageLock{
		{"envoy", "docker.io/envoyproxy/envoy", "v1.39.0", "sha256:d59f7f5fa10cff6d5892b6c5e7df5c9297ddfb2c3683e33fbfb82da24de4fa66"},
		{"openflow", "nvcr.io/nvstaging/doca/doca-tower-openflow", "0.1.10", "sha256:7019071feb4fba5e5123efad56e0cda640d4b7edf4b8129f2238b26dde0f189a"},
		{"plugin", "nvcr.io/nvstaging/doca/doca-tower-policy-plugin", "0.2.4", "sha256:06db03322fde0971bc13b83ff4f43f97212e6fb49963b30d0ef7de8ebd9fa8a2"},
		{"tap", "nvcr.io/nvstaging/doca/doca-tower-tap", "0.2.5", "sha256:59617284dee716701c7d1540bde2ba64a6a5dfdd1fd136aa12339c6a8f7c561a"},
		{"controller", "nvcr.io/nvstaging/doca/doca-tower-policy-sidecar", "0.2.5", "sha256:6c2f18fa661394d01cbbabe6be7d160da573ec526db971f5ff0a271eaac7371c"},
		{"server", "nvcr.io/nvstaging/doca/doca-tower-policy-rest", "0.2.5", "sha256:e1861e1bc153c249c9d07d05bf2d034870dee04afec9498d63b26c24cc9c1843"},
		{"argus", "nvcr.io/nvstaging/doca/doca_argus_ai_gateway", "20261017", "sha256:e216a90398052b6ba177f5400e0ab462a711b7f242f44246404f412f20d25ff2"},
	}
}
