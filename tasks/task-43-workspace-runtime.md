# Task 43: Production Workspace Runtime

Status: Ready for design; implementation depends on ADR decision. Priority: P1.

## Goal

Replace the production gap behind the current fake-only Resource Controller with an AWS Runtime that implements ensure-running, suspend, resume, and delete without weakening the existing Workspace revision-fencing contract.

## Work

- First record an ADR for the concrete MicroVM/compute provider and its lifecycle/API limits; do not assume that a Lambda function invocation itself provides a durable interactive Workspace.
- Implement the `Runtime` interface with provider API clients, explicit per-operation deadlines, operation idempotency, and durable revision/tombstone fencing before side effects.
- Persist/re-discover provider IDs via tagged Resource Bindings and repair DB/provider drift without taking over unrelated resources.
- Compensate partial provisioning and make delete idempotent; preserve failed cleanup state for retries and operator visibility.
- Keep Resource Plane credentials in IAM roles and never move provider credentials into Control Plane or user clients.

## Acceptance criteria

- Provider fake tests cover duplicate operations, stale revisions, concurrent operations, partial create failure, retry, delete tombstones, and foreign resources.
- Integration tests cover actual Runtime adapter against a local fake provider API; no AWS apply is implicit.
- IAM and tags are minimal and follow `docs/aws-resource-tags.md`.
- Operational boundaries, provider quotas, suspend/resume semantics, and unsupported runtime classes are documented.
