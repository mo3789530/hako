# Task 57: Tenant GitHub Credential Resolver and Cache Isolation

残作業の実行単位：[Task 69](task-69-tenant-checkout-broker.md)。親タスクは集約用に保持し、子タスク完了だけで配備済みとはしない。

Status: In Progress (Tenant App proof now has a Control Plane-only mTLS API→Broker path. The Broker re-resolves the draft's exact App/revision/generation and approved Role/Secret/Version from PostgreSQL before invoking the GitHub `GET /app` proof client. Resource references are not sent by the API. API/Broker wiring and a PostgreSQL-backed fake-verifier route test pass locally; production certificate/IAM/network provisioning remains). Priority: P0.

## Goal

Extend Task 37's internal credential library using [Tenant App design sections 4/5/7](../docs/tenant-github-apps.md) without giving Resource Plane/Job/API roles access to Tenant private keys.

## Work

- Resolve App/integration/approved binding from authoritative Tenant/Run context; validate active ownership, Installation/Repository integration, expected revision/generation. Never trust caller-supplied App ID or arbitrary Secret/Role.
- Implement trusted purpose-specific Secret provider/reader-role factory with server-approved Role ARN, exact Secret ARN and pinned version for owned mode. Keep shared AWSCURRENT compatibility.
- Add `GET /app` proof of App ID for draft verification. Separate external verification from DB transactions and recheck revision before accepting ready status.
- Partition bounded client/token/verifier pools by Tenant/integration/App/revision/generation; maintain read-only repository scope, pre-expiry refresh, serialized minting, secret-safe errors, and App-scoped cooldown.
- Revalidate generation on every use, invalidate stale pools on changes and fail closed. No shared/legacy fallback after owned failure.

## Acceptance / tests / docs

Progress: `TokenClient.VerifyApp` signs with the configured App key, calls GitHub `GET /app`, checks the returned App ID, and returns only fixed safe credential errors. Draft and active resolvers return the exact approved binding, reader Role ARN, private-key Secret ARN and immutable Version ID only for the expected Tenant/App/revision/generation. `TenantPrivateKeySecretAPI` assumes only the exact Role with a session policy restricted to `GetSecretValue` on that exact Secret and requests the pinned VersionId. Tenant-scoped `TokenClient` revalidates active identity before cached-token reuse and after remote calls. Task 59 persists the resolved identity on each Run; Task 69 adds the local trusted mTLS checkout broker and strict owned/shared provider routing. The API binary still does not directly read Tenant private keys. Broker process/certificate lifecycle, worker wiring and deployed IAM remain outstanding.

Tenant App draft verification is now wired separately from OAuth setup and
checkout. When configured, the API sends only Tenant, integration, App ID,
config revision, and credential generation to the Control Plane mTLS Broker.
The Broker resolves the approved proof credential from PostgreSQL, rejects a
stale or mismatched App pin, then uses the exact reader Role, Secret ARN, and
immutable Secret VersionId to call GitHub `GET /app`. The proof API is optional
and remains fail-closed when its dedicated mTLS configuration is absent. Unit,
strict-route, API startup-configuration, and local PostgreSQL composition tests
use a fake proof callback; they do not contact AWS, Secrets Manager, or GitHub.
Run the focused acceptance with `make test-tenant-app-proof-integration`.

- Generated RSA keys, fake Secret/STS/GitHub providers test A/B isolation, correct Role/ARN/version, mismatched App proof, rotation, revoked/suspended configuration, concurrent requests and stale client/cache.
- Tokens/keys cannot appear in DB/messages/errors/artifacts; no public token endpoint is added.
- Document broker blast radius and exact IAM contract. Deployment/policy resources are Task 59; real secrets are not retrieved here.
