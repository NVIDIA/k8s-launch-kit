// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package app owns DPF discover, generate and deploy operation composition.
// Pure generation and retained-input resolution remain separate from explicit
// artifact, access, collection and publication I/O. Target adapters own process
// context and final presentation; deployment owns lifecycle stage execution.
package app
