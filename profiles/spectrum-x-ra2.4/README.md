# Spectrum-X RA2.4

This profile targets Network Operator 26.10 and newer catalogued releases.
Use a full doSPCX ConfigMap with `data.format: dospcx-data.tar.gz/v1` and
`binaryData.dospcx-data.tar.gz`. It is rendered once in the operator namespace;
NCO receives version RA2.4 and each source group's derived platformType.

Platform selection uses the longest case-insensitive doSPCX platform substring
in gpuType, without requiring a preset. A machineType containing VR
(case-insensitive) takes precedence and selects vr, including without a GPU product. Unresolved discovery warns and saves;
unresolved selected RA2.4 generation fails. Optional spectrumX.ovsConfig is a
string map. Per-group spectrumX.swPlaneByRail assigns offsets to existing
HWPLB rails; merged groups must agree. Multiple hardware partitions within a
logical rail require a separate topology model.

Network Operator v26.10.0-beta.2 does not yet package the NCO platformType CRD.
Deployment requires the doSPCX NCO implementation and matching CRDs. Deploy
checks the live served NCT schema after policy bootstrap and fails before
applying the bundle/NIC templates if platformType is unavailable or inspection
fails, including dry-run and externally managed Helm. See the
[published guide](../../docs/user/spectrum-x.md#ra24-dospcx-profile) for full
prerequisites, addressing and DRA settings.

Generation supports a full compatible GPU/rail-count cohort or a single-source
filter. Multiple rail-pool parents are rejected because their operator-created
child policies and OVSNetworks would reuse rail names. A merged GPU selector
must not include excluded same-GPU sources in other buckets; select one source
with `--groups` when that would happen.

Every selected source needs complete dense rail IDs, uniform NICs per rail,
and an integral planes-per-NIC ratio. A physical NIC cannot span rails. Merged
sources must share that layout. This excludes the current
`ThinkSystem-SR650-V4-RTX-PRO-6000` preset, which splits one NIC across rails.
Discovery refresh retains explicit software-plane assignments only for verified
unchanged worker membership and rail hardware; otherwise it drops them and warns.

VR generation supports IPv4 and IPv6 HWPLB with eight planes on four dual-NIC rails.
It emits four per-rail naming templates (`pfsPerNic: 4`), eight per-NIC
`railRnicN` topology entries and CIDRPools, and eight workload attachments.
Each NIC endpoint has four PFs, one schedulable VF and its own IPv4 /31 or IPv6 /64. DRA uses
one claim per logical rail with one GPU and two VF requests. NIC input order
is preserved; it must agree with the physical switch topology. RDMA names
are `roce_rR_n0`/`roce_rR_n1`; custom RDMA prefixes require `%nic_id%` and no
plane placeholder. Compatible NCO must support disjoint PCI naming templates.
Reference topology JSON must include all eight planes per rail and explicit
zero-based host `attributes.host_index`; AIR uses its parsed host ordinal.
VR IPv4 defaults to site-zero `240.0.0.0/8`, with NIC(1), rail(2), POD(5),
SU(7), host(8), peer(1) bits after the first octet. Eight-plane POD is always
zero and host index is at most 251. Pools are per NIC/rail /11, allocations
are /31 with even host and odd gateway, and routes cover /11 and /8.
VR IPv6 uses `fd02:000S:NRPP:UUHH::peer/64`: site nibble `S`, NIC nibble
`N`, rail nibble `R`, POD byte `PP`, SU byte `UU`, and host byte `HH`.
`profile.spectrumX.ipv6SiteID` selects site 0..15 (default 0; YAML-only).
The two NICs use distinct per-NIC/rail /40 pools; each host gets a static /64,
with host `::1`, gateway `::2` (excluded from allocation), and route `fd02::/28`.
POD remains zero, SU is 0..127, and host index is 0..251. `hostFirstOctet`
affects only IPv4; nonzero `ipv6SiteID` is rejected outside VR IPv6.
Non-HWPLB VR, incomplete topology, and extra software-plane offsets
are rejected. Generation does not qualify runtime support or configure switches.

See [VR generation](../../docs/user/spectrum-x.md#vr-dual-nic-rails) for topology input and migration requirements.
