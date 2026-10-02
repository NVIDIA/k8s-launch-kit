## ADDED Requirements

### Requirement: NIC configuration success condition

Deploy and validate SHALL accept NIC configuration convergence only when each matched device has current desired configuration and a current ConfigUpdateInProgress condition with reason UpdateSuccessful and status False. Existing firmware gates SHALL still apply when firmware configuration is present.

#### Scenario: Contradictory success condition

- **WHEN** a matched NicDevice reports UpdateSuccessful but ConfigUpdateInProgress is True, Unknown or absent
- **THEN** configuration is not classified as successful.

#### Scenario: All selected devices converged

- **WHEN** all matched devices carry current configuration and ConfigUpdateInProgress=False with reason UpdateSuccessful and any required firmware gate passes
- **THEN** deploy and validate classify the template as ready.
