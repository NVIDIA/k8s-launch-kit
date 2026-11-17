// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package app composes DPF generation input resolution, artifact retrieval and
// caller-permission inspection. ResolveGeneration and retained input binding are
// pure; public-file loading, chart transport and management authorization perform
// explicit I/O. Target adapters own process context, publication and final output.
// Discovery collector assembly and deployment execution remain separate callers.
package app
