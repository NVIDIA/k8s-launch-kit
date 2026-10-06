# Spectrum-X RA2.4

This profile targets Network Operator 26.10 and newer catalogued releases.
Use a full doSPCX ConfigMap with `data.format: dospcx-data.tar.gz/v1` and
`binaryData.dospcx-data.tar.gz`. It is rendered once in the operator namespace;
NCO receives version RA2.4 and each source group's derived platformType.

Platform selection uses the longest case-insensitive doSPCX platform substring
in gpuType, without requiring a preset. Unresolved discovery warns and saves;
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
