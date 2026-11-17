// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package app composes DPF generation input resolution, artifact retrieval and
// caller-permission inspection and shared discovery collectors. ResolveGeneration and retained input binding are
// pure; public-file loading, chart transport and management authorization perform
// explicit I/O. Native deployment prerequisites collect, publish and compare fresh
// observations with the retained plan. Target adapters own process context,
// standalone discovery publication and final output; deployment owns stage execution.
package app
