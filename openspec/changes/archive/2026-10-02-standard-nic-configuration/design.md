# Design

## Decisions

- The default is false. A pointer bool CLI option uses the existing tag-driven root/generate/discover binding, preserving explicit false. Deploy/validate consume generated resources and resolved bundle metadata, not a generation override.
- A shared template renders once per original source group using the existing NicConfigurationTemplate scope. NIC type and PCI addresses are restricted to east-west PFs; missing/mixed types or missing PCI selectors fail instead of broadening selection. Source nodeSelector is mandatory. Stable resource identities are retained under subset selection.
- `numVfs` comes from `sriov.numVfs` for all standard profiles; linkType follows resolved profile.fabric (Ethernet or Infiniband). PCI maxReadRequest=4096 and Baremetal GPUDirect tuning apply to both; only Ethernet gets DSCP trust and priority-3 PFC.
- Enable NCO in NicClusterPolicy independently of naming/firmware-download flags. Standard SR-IOV Helm values and OCP SriovOperatorConfig disable mellanox only when opted in. Other profile families do not change SR-IOV operator settings.
- OCP merges requested disabled plugins into existing values. Turning the flag off omits the field and preserves externally owned settings; it does not re-enable a plugin disabled for another reason. Kubernetes Helm retains its existing values ownership policy; skipHelmChart leaves chart configuration external.
- Existing deployment phases apply NCO through NCP before templates, then apply remaining resources before registry verification. The existing registry handles matched devices, expected payload, generation freshness, partial progress and errors for both deploy and validate. Tighten its success case to require status False in addition to UpdateSuccessful.
- Spectrum-X rejects the new opt-in to avoid a conflicting second owner/template. No Spectrum-X profile changes.

## Risks and limits

NIC changes can require reboot; existing maintenance/reconciliation applies. Runtime RoCE support depends on the installed NCO/DOCA/RHCOS combination. Offline tests do not qualify that combination. NCO configures whole devices: selected PFs must belong to NICs approved for configuration. Existing cleanup includes NicConfigurationTemplate; OCP cleanup remains manual and externally installed operators remain external.

## Verification

Test omitted/true/false precedence and root/generate/discover routing; render every standard profile/fabric/flavor in enabled and disabled modes; verify typed payload and exact selectors, subset scope, independent NCO enablement, SR-IOV-only plugin setting and Spectrum-X rejection. Test OCP plugin union/idempotence and shared deploy/validate registry classification, including contradictory status and stale/partial evidence. Run build, unit/race tests, lint, documentation checks and strict OpenSpec validation. No real-cluster discovery, deploy or validate.
