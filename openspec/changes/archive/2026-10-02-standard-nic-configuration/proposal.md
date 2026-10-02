# Proposal

## Why

Standard networking profiles can enable NIC Configuration Operator for naming, but cannot request the NIC firmware and runtime tuning needed for RDMA. Add an explicit opt-in that coordinates configuration ownership and monitors device convergence on Kubernetes and OpenShift.

## What Changes

- Add default-false `nicConfigurationOperator.deployNicConfigurationTemplate` and `--deploy-nic-configuration-template` on root, generate and discover.
- Render scoped NIC configuration for standard SR-IOV, RDMA-shared and host-device profiles, including Ethernet RoCE tuning and InfiniBand without RoCE tuning.
- Only SR-IOV profiles disable the Mellanox plugin; preserve unrelated OCP disabled plugins.
- Require current matched NicDevices to report ConfigUpdateInProgress=False with UpdateSuccessful for configuration success.
- Leave Spectrum-X templates and configuration ownership out of scope; reject this new opt-in with Spectrum-X.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `profile-planning`: Opt-in standard NIC configuration with coordinated SR-IOV ownership and exact hardware selection.
- `resource-kinds`: Explicit configuration success condition for matched NicDevices.

## Impact

Config schema/defaults and tagged CLI routing; ten standard profile variants and one shared template; OCP configuration merge; shared readiness classifier. Existing NCO APIs and registry scopes are reused. No dependency changes or real-cluster execution. Configuration presence, platform ownership and managed-effects contracts also apply.
