## 1. Configuration and rendering

- [x] 1.1 Add default-false config field, CLI routing, presence tests and Spectrum-X exclusion.
- [x] 1.2 Render scoped templates for all standard profiles and both fabrics; enable NCO independently.
- [x] 1.3 Disable Mellanox only for opted-in SR-IOV and preserve OCP unrelated plugins.

## 2. Readiness and regression coverage

- [x] 2.1 Require the exact success condition and test deploy/validate integration.
- [x] 2.2 Cover full/subset hardware scope, opt-out, all profile variants and invalid inputs.

## 3. Documentation and delivery

- [x] 3.1 Update configuration, CLI, profiles, OpenShift, validation and relevant skill guidance.
- [x] 3.2 Run offline build/tests/race/lint/docs/OpenSpec checks and inspect final diff.

## Evidence

- CLI/config: `TestNicConfigurationFlagResolutionAcrossCommands`, `TestNicConfigurationRejectsSpectrumX`, Host request snapshot test, command flag-ownership/help tests.
- Rendering/selection: `TestStandardNicConfigurationProfiles` (48 fabric/flavor/enablement/multirail cases, including typed Helm paths), `TestStandardNicConfigurationInvalidSelection`, `TestStandardNicConfigurationSubsetScope`.
- Ownership: `TestOCPDisabledPluginsPreservedAndValidated` covers additive merge, idempotence, unrelated settings, opt-out and malformed values.
- Readiness: `TestStandardNicConfigurationDeployValidateConvergence` exercises the actual deploy poll and standalone validation against fake API state on both flavors; existing matched-device, payload, generation and partial-device tests remain passing. Contradictory success statuses are covered in `TestNicConfigurationTemplate_AllReasonsClassified`.
- Offline gates: build, full race suite (follow-up command tests after fixing the new flag's help group), lint, documentation contract checks, strict MkDocs, strict OpenSpec validation.
- No real cluster tests. Hosted CI and review are tracked by the PR, separately from this implementation checklist.
