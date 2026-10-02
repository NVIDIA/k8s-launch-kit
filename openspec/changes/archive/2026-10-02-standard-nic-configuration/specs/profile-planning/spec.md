## ADDED Requirements

### Requirement: Opt-in standard NIC configuration

Standard SR-IOV, RDMA-shared and host-device profiles SHALL support a default-disabled NIC configuration template option on Kubernetes and OpenShift. Enabled templates SHALL target each selected source group's east-west NIC type and PCI addresses, route VF count from SR-IOV settings and link type from the resolved fabric, and configure PCI and Baremetal GPUDirect optimizations. Ethernet SHALL additionally configure DSCP trust and priority-3 PFC; InfiniBand SHALL omit RoCE settings. Only SR-IOV profiles SHALL disable the Mellanox plugin, preserving unrelated OCP disabled plugins. Spectrum-X SHALL reject the new option.

#### Scenario: Default or explicit opt-out

- **WHEN** the option is omitted or explicitly false
- **THEN** no standard NIC configuration template or new plugin-disable request is rendered.

#### Scenario: Profile-owned NIC configuration templates

- **WHEN** a standard profile loads its NIC configuration template
- **THEN** it loads a template file from its own directory without referencing another profile.

#### Scenario: Standard profile selection

- **WHEN** the option is enabled for a supported standard profile and selected hardware groups
- **THEN** NCO is enabled independently of naming and templates retain exact per-source selectors, fabric-appropriate settings and configured VF count.

#### Scenario: Operator configuration ownership

- **WHEN** enabled SR-IOV configuration is applied to OpenShift with other plugins already disabled
- **THEN** mellanox is added without removing the existing disabled plugins; RDMA-shared and host-device profiles do not request this operator setting.

#### Scenario: Spectrum-X conflict

- **WHEN** the new option is enabled with Spectrum-X
- **THEN** configuration is rejected without changing Spectrum-X templates.

#### Scenario: Whole-NIC scope protection

- **WHEN** an enabled standard template selects an east-west PF whose source inventory includes an excluded PF on the same PCI device
- **THEN** generation rejects the configuration before filtering out excluded PFs; unselected source groups do not affect the selected subset.

#### Scenario: Previously deployed template omitted

- **WHEN** opt-out or subset generation omits a previously deployed standard NIC configuration template
- **THEN** Kubernetes deploy preflight treats it as a stray, blocking unless overwrite-existing authorizes deletion; OCP preserves it, and neither platform automatically restores applied NIC settings.
