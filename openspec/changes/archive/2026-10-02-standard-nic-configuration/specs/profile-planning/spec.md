## ADDED Requirements

### Requirement: Opt-in standard NIC configuration

Standard SR-IOV, RDMA-shared and host-device profiles SHALL support a default-disabled NIC configuration template option on Kubernetes and OpenShift. Enabled templates SHALL target each selected source group's east-west NIC type and PCI addresses, route VF count from SR-IOV settings and link type from the resolved fabric, and configure PCI and Baremetal GPUDirect optimizations. Ethernet SHALL additionally configure DSCP trust and priority-3 PFC; InfiniBand SHALL omit RoCE settings. Only SR-IOV profiles SHALL disable the Mellanox plugin, preserving unrelated OCP disabled plugins. Spectrum-X SHALL reject the new option.

#### Scenario: Default or explicit opt-out

- **WHEN** the option is omitted or explicitly false
- **THEN** no standard NIC configuration template or new plugin-disable request is rendered.

#### Scenario: Standard profile selection

- **WHEN** the option is enabled for a supported standard profile and selected hardware groups
- **THEN** NCO is enabled independently of naming and templates retain exact per-source selectors, fabric-appropriate settings and configured VF count.

#### Scenario: Operator configuration ownership

- **WHEN** enabled SR-IOV configuration is applied to OpenShift with other plugins already disabled
- **THEN** mellanox is added without removing the existing disabled plugins; RDMA-shared and host-device profiles do not request this operator setting.

#### Scenario: Spectrum-X conflict

- **WHEN** the new option is enabled with Spectrum-X
- **THEN** configuration is rejected without changing Spectrum-X templates.
