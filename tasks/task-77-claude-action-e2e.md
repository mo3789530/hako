# Task 77: Claude Action local E2E, operations and security evidence

Status: Complete (local acceptance; production rollout remains a separate deployment gate). The focused local/CI PostgreSQL lifecycle starts with a rejected tampered signature, then uses the production Tenant webhook HTTP handler and DB-backed Installation authorizer for signed queued, runner-assignment and terminal deliveries. It admits and leases the job, runs the production provenance verifier against synthetic authoritative metadata, runs the startup worker with fake Lambda MicroVM/token/bootstrap providers, dispatches all durable Check Outbox revisions through the production Dispatcher/Checks consumer to a fake GitHub Checks API, then recovers Broker-mediated physical cleanup after each provider fails once. Success/failure/cancel/timeout, duplicate terminal deliveries, signature rejection, lease fencing, unknown integration/wrong job denial, revoked App denial and generation rotation during Check token mint are covered. A second Tenant has its own GitHub App and model credential; its integration cannot resolve the first Tenant's runner job or reach the fake Secret reader. Fork, `pull_request_target`, foreign head repository, cross-Tenant provenance and wrong-SHA metadata are rejected before model Secret retrieval. The production model credential resolver delivers only a synthetic Tenant A credential to the bound Lambda MicroVM bootstrap. Setup, workflow configuration, rotation/disable, audit, incident response, CI invocation and deployment gates are documented. The official digest-pinned ARM64 runner image builds locally and has been inspected; image execution, live GitHub Actions API, and live AWS/GitHub deployment are not verified and are not part of local acceptance. Priority: P2. Parent: Task 73. Depends on: Tasks 74–76.

## Goal

Provide a repeatable local/fake end-to-end proof and operator documentation for the Claude Code Action path without requiring real Claude API usage or modifying GitHub/AWS configuration.

## Work

- [x] Compose authorized queued admission → runner startup/bootstrap → GitHub assignment/completion → durable Check → verified retryable cleanup using local PostgreSQL and fake providers.
- [x] Compose signed HTTP webhook ingress and outgoing fake-GitHub Checks API publication into that same end-to-end test; verify a tampered signature cannot enter the inbox.
- [x] Verify the DB-backed Checks credential resolver refuses unknown integration, wrong job and revoked App before token mint, and fences generation rotation during mint; cancellation and cleanup failures are covered in the composed lifecycle.
- [x] Compose fork/untrusted PR, cross-Tenant secret access and wrong-SHA provenance denials into the end-to-end path; verify denial occurs before the fake Secret reader is called.
- [x] Document Tenant setup, supported workflow configuration, credential rotation/disable, audit evidence, incident response, and production rollout gates.
- [x] Add CI invocation and retain only redacted, non-secret test evidence.

## Acceptance criteria

- One command runs the local E2E against fake external services and PostgreSQL without stopping the shared local database.
- Tests prove no secret is resolved for denied jobs and no credential, runner or MicroVM remains after terminal completion or retryable cleanup failure.
- Security documentation distinguishes tested local behavior from unverified AWS IAM, Lambda MicroVM image/network behavior and external GitHub settings.
- Real model API calls, live GitHub installation changes, secret provisioning and AWS apply remain out of scope unless explicitly approved.

## Additional local transport evidence (2026-10-24)

`TestActionsRunnerModelCredentialUsesVerifiedResourcePlaneMTLS` performs a
real TLS 1.3 handshake between the production model-credential HTTP client and
Broker handler using ephemeral CA-issued test certificates. The allowlisted
Resource Plane receives a synthetic credential; another trusted but
non-allowlisted Plane and a client without a certificate are rejected before
the credential issuer runs. Production certificate lifecycle, AWS networking,
and Lambda MicroVM execution remain unverified.
