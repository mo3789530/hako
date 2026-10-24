# Task 75: Claude Tenant credential and trust policy

Status: Local acceptance complete; production model-credential IAM allowlist is represented in Terraform, while target-role provisioning, certificate deployment, and service deployment remain. Tenant API/CLI, exact-version reader and fenced resolver are implemented. A dedicated Resource Plane mTLS endpoint, Lambda Worker client, signed MicroVM-bound bootstrap handoff, guest `/run` identity binding, single-use receiver, and official-runner process environment handoff are locally tested. The ARM64 guest image builds on the official digest-pinned Runner base; the composed PostgreSQL E2E proves the synthetic key reaches only the Runner process and is removed with guest work state. The image was not executed because this x86_64 host has no ARM64 emulator. Priority: P2. Parent: Task 73. Depends on: Tasks 50, 56–60, 74.

## Goal

Make model credentials Tenant-owned, purpose-specific and available only to explicitly trusted Claude Action jobs. A Tenant's model credential is distinct from GitHub App credentials and AWS workload identity.

## Work

- [x] Define a Tenant-scoped secret reference and permission model for Claude/provider credentials, including create/rotate/disable metadata without storing plaintext in the control-plane database.
- [x] Add an exact-Role/Secret/Version AWS provider with a session policy limited to `GetSecretValue` on one ARN. No AWSCURRENT fallback; tests use fakes only.
- [x] Add a trusted resolver that requires launch-time provenance hash, live claim, exact Plane/MicroVM, active Tenant credential, and approved same-Tenant binding; recheck after Secret I/O and zero the buffer after in-process consumption.
- [x] Deny cross-Tenant binding references, unverified/fork jobs, stale policy, disabled credentials, and rotation races before delivering a value. Public metadata API is Owner/Admin only; default is no credential.
- [x] Add a Plane-allowlisted broker endpoint/client and signed, short-lived, exact-MicroVM bootstrap handoff; keep the value out of SQS, GitHub runner registration token, command arguments, logs, artifacts, and public Tenant API responses.
- [x] Implement a bounded, signature/VM/expiry-verifying, single-use guest receiver that wipes the model-key byte buffer after synchronous consumption.
- [x] Add a guest executable/process consumer that configures an ephemeral official runner without the model key, supplies the key only to the runner process environment, and removes guest-local credential/work state after exit.
- [x] Build the pinned ARM64 runner image and verify the broker→Worker→guest process path with a synthetic key; assert the key reaches only the runner environment and guest credential/work state is removed. The image artifact was inspected, not executed on this x86_64 host.
- Keep GitHub checkout/write permissions separate from model API credentials; default Claude job repository access is read-only unless a separately approved write capability is requested.
- [x] Specify redaction, timeout, rotation, revocation, operator provisioning, and incident response boundaries in `docs/claude-model-credentials.md`.
- [x] Add a production Broker IAM input for exact Tenant model credential reader Roles, separate from GitHub private-key and Webhook reader purposes; validate default-off, purpose-specific paths, exact ARNs, and non-overlap in Terraform mock plans. This is policy configuration only; no target Role, Secret, or certificate is provisioned.
- [ ] Provision and review target Tenant reader Role trust/identity policies and required KMS grants in the owning Resource Plane/account. No AWS apply or real Secret retrieval is included in local implementation.
- [ ] Provision Broker mTLS certificates and add production Broker/Worker deployment configuration after the final runtime delivery protocol is reviewed.

## Acceptance criteria

- Unit and PostgreSQL tests prove cross-Tenant secret reference/authorization denial and that disable/rotation takes effect for new runs.
- Fake provider tests prove untrusted/fork jobs cannot trigger secret resolution and trusted jobs receive only the configured provider credential.
- Secret values never appear in API responses, DSQL, logs, command arguments, runner registration data or stored artifacts.
- IAM/secret-reader purpose and trust boundaries are documented; no real Secret is read or provisioned in this task.
- Terraform grants the Broker only exact `sts:AssumeRole` targets for each configured purpose and keeps model credentials distinct from GitHub App private-key and Webhook roles.
