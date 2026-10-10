# Task 57: Tenant GitHub Credential Resolver and Cache Isolation

Status: In Progress (App proof API, active/draft revision/generation resolver, exact reader provider, per-use active generation authorization/cache partition, and durable Run identity pin/provider contract implemented; production Run→AWS credential broker wiring remains). Priority: P0.

## Goal

Extend Task 37's internal credential library using [Tenant App design sections 4/5/7](../docs/tenant-github-apps.md) without giving Resource Plane/Job/API roles access to Tenant private keys.

## Work

- Resolve App/integration/approved binding from authoritative Tenant/Run context; validate active ownership, Installation/Repository integration, expected revision/generation. Never trust caller-supplied App ID or arbitrary Secret/Role.
- Implement trusted purpose-specific Secret provider/reader-role factory with server-approved Role ARN, exact Secret ARN and pinned version for owned mode. Keep shared AWSCURRENT compatibility.
- Add `GET /app` proof of App ID for draft verification. Separate external verification from DB transactions and recheck revision before accepting ready status.
- Partition bounded client/token/verifier pools by Tenant/integration/App/revision/generation; maintain read-only repository scope, pre-expiry refresh, serialized minting, secret-safe errors, and App-scoped cooldown.
- Revalidate generation on every use, invalidate stale pools on changes and fail closed. No shared/legacy fallback after owned failure.

## Acceptance / tests / docs

Progress: `TokenClient.VerifyApp` signs with the configured App key, calls GitHub `GET /app`, checks the returned App ID, and returns only fixed safe credential errors. The API reads draft proof metadata transactionally, calls an injected verifier outside DB transactions, then conditionally rechecks revision/generation and transitions to `ready`; failed proof does not change state. The normal API binary deliberately does not inject a direct AWS verifier: it cannot read Tenant private-key Secrets or assume key-reader roles, so verification remains unavailable until a trusted broker adapter is deployed. Draft and active resolvers return the exact approved binding, reader Role ARN, private-key Secret ARN and immutable Version ID only for the expected Tenant/App/revision/generation. `TenantPrivateKeySecretAPI` is a purpose-limited library for the trusted credential service; it assumes only the exact Role with a session policy restricted to `GetSecretValue` on that exact Secret, requests the pinned VersionId and rejects mismatched versions. Tenant-scoped `TokenClient` partitions cache identity by Tenant/integration/App/revision/generation and requires an authorizer that revalidates active integration state before cached-token reuse and after remote calls. Task 59 now persists the resolved identity on each Run, and checkout refuses to pass an owned Run to the shared legacy provider. Production proof/checkout broker, worker wiring and deployed IAM remain outstanding.

- Generated RSA keys, fake Secret/STS/GitHub providers test A/B isolation, correct Role/ARN/version, mismatched App proof, rotation, revoked/suspended configuration, concurrent requests and stale client/cache.
- Tokens/keys cannot appear in DB/messages/errors/artifacts; no public token endpoint is added.
- Document broker blast radius and exact IAM contract. Deployment/policy resources are Task 59; real secrets are not retrieved here.
