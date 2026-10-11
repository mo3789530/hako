# Task 75: Claude Tenant credential and trust policy

Status: Todo. Priority: P2. Parent: Task 73. Depends on: Tasks 50, 56–60, 74.

## Goal

Make model credentials Tenant-owned, purpose-specific and available only to explicitly trusted Claude Action jobs. A Tenant's model credential is distinct from GitHub App credentials and AWS workload identity.

## Work

- Define a Tenant-scoped secret reference and permission model for Claude/provider credentials, including create/update/rotate/disable metadata without storing plaintext in the control-plane database.
- Resolve secrets only inside the trusted execution boundary using a narrowly scoped role/reference; never return plaintext to public API/CLI or include it in queue payloads, artifacts or logs.
- Deny credentials to fork/untrusted PRs, unapproved workflow refs, unauthorized Tenant members and jobs without explicit policy. Default policy is no model credential.
- Keep GitHub checkout/write permissions separate from model API credentials; default Claude job repository access is read-only unless a separately approved write capability is requested.
- Specify redaction, timeout, rotation, revocation and incident response behavior.

## Acceptance criteria

- Unit and PostgreSQL tests prove cross-Tenant secret reference/authorization denial and that disable/rotation takes effect for new runs.
- Fake provider tests prove untrusted/fork jobs cannot trigger secret resolution and trusted jobs receive only the configured provider credential.
- Secret values never appear in API responses, DSQL, logs, command arguments, runner registration data or stored artifacts.
- IAM/secret-reader purpose and trust boundaries are documented; no real Secret is read or provisioned in this task.
