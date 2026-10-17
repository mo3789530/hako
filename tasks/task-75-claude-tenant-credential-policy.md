# Task 75: Claude Tenant credential and trust policy

Status: In Progress (Tenant-scoped metadata API/CLI, operator-only binding schema, exact-version AWS secret reader, and a fakeable provenance/MicroVM-fenced resolver are implemented locally. Runtime delivery into the actual Action process and deployment IAM wiring remain). Priority: P2. Parent: Task 73. Depends on: Tasks 50, 56–60, 74.

## Goal

Make model credentials Tenant-owned, purpose-specific and available only to explicitly trusted Claude Action jobs. A Tenant's model credential is distinct from GitHub App credentials and AWS workload identity.

## Work

- [x] Define a Tenant-scoped secret reference and permission model for Claude/provider credentials, including create/rotate/disable metadata without storing plaintext in the control-plane database.
- [x] Add an exact-Role/Secret/Version AWS provider with a session policy limited to `GetSecretValue` on one ARN. No AWSCURRENT fallback; tests use fakes only.
- [x] Add a trusted resolver that requires launch-time provenance hash, live claim, exact Plane/MicroVM, active Tenant credential, and approved same-Tenant binding; recheck after Secret I/O and zero the buffer after in-process consumption.
- [x] Deny cross-Tenant binding references, unverified/fork jobs, stale policy, disabled credentials, and rotation races before delivering a value. Public metadata API is Owner/Admin only; default is no credential.
- [ ] Wire credential delivery to the actual Action process in the trusted runtime without putting plaintext in SQS, runner registration data, command arguments, logs, artifacts, or API responses.
- Keep GitHub checkout/write permissions separate from model API credentials; default Claude job repository access is read-only unless a separately approved write capability is requested.
- [x] Specify redaction, timeout, rotation, revocation, operator provisioning, and incident response boundaries in `docs/claude-model-credentials.md`.
- [ ] Add production broker IAM allowlist and deployment configuration after reviewing the final runtime delivery protocol.

## Acceptance criteria

- Unit and PostgreSQL tests prove cross-Tenant secret reference/authorization denial and that disable/rotation takes effect for new runs.
- Fake provider tests prove untrusted/fork jobs cannot trigger secret resolution and trusted jobs receive only the configured provider credential.
- Secret values never appear in API responses, DSQL, logs, command arguments, runner registration data or stored artifacts.
- IAM/secret-reader purpose and trust boundaries are documented; no real Secret is read or provisioned in this task.
