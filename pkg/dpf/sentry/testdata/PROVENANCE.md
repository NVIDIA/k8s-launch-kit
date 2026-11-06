<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

These small archive fixtures are the authentic NVIDIA-owned Sentry charts used
by the offline projection tests. They were retrieved on 2026-11-06 from the
coordinates and SHA-256 identities in catalog.go. They contain templates and
public image coordinates, with no registry credentials or lab observations.

The profile templates derive from gzaidman/doca-sentry-deploy commit
0f3c75e54a7f596fc0c469113d55996562975c39. The adaptation removes example OS
password/login configuration and the lab BMC reboot script. It supplies explicit
instance names, selected tenant and device scope, existing Secret names and
immutable artifact/image identities. Host/DPU registration and base DPF
installation remain external prerequisites. This engineering provenance is not
an assertion of release support or runtime qualification.

Core policy uses ext_authz and Argus with the source chart's `tap.enabled=false`.
It needs no inference Secret and projects four Tower containers. Opting into
LLM inspection enables the fifth `tower-tap` container and requires the real
`INFERENCE_API_KEY` Secret key. The upstream chart archives are unmodified:
both listeners retain an optional ext_proc hook with failure_mode_allow=true,
including when its sidecar is disabled. Core policy therefore makes no claim
of LLM inspection; the independent ext_authz filters remain fail-closed in
both modes. Runtime positive/negative policy tests are still required.

Offline workload projections use the DPF controller's effective Helm inputs,
separately from the native DPUService chart inputs. The standard-service path
is identical in DPF26.8.0-rc.1 (`6bc1bd3f00f5fb4064600748da7164e3610622cd`)
and26.8.0 (`fc4974ed3bc026ae27634673c2fdf818a2972235`):

- `internal/dpuservice/controllers/dpuservice_controller.go:458-513,659-716`
  resolves service interfaces and NAD resource names, injecting resource counts.
- `internal/dpuservice/controllers/nad.go:33-72,110-143` merges existing
  nonempty network fields, adds the invalid-network sentinel and sorts by
  network name/interface. Multus v4.0.2 serialization includes `cni-args:null`.
- `internal/dpuservice/controllers/dpuservice_controller.go:1706-1780,1854-1860`
  adds the explicit service-ID label and merges generated daemonset values with
  raw chart values, using generated values as authoritative.
- `internal/dpuservice/utils/mergemaps.go:26-68` defines that merge precedence.

`values.go` implements only the Sentry standard-service branch and its declared
NAD dependencies. It rejects unsupported annotation fields, system-component
and config-port branches instead of pretending to project them. It does not
import later-main update-strategy defaults. The sentinel belongs in Helm's
DaemonSet template; the pod-IPAM webhook removes it from Pods later. Chart
projections neither model that webhook nor submit its output as native inputs.
