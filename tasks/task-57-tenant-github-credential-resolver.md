# Task 57: Tenant GitHub Credential Resolver and Cache Isolation

Status: In Progress (App ID proof and revision/generation-bound draft resolver with exact reader Role/Secret Version metadata implemented; STS-backed Secret provider, integration-scoped cache, verify API and Run integration binding remain). Priority: P0.

## Goal

Extend Task 37's internal credential library using [Tenant App design sections 4/5/7](../docs/tenant-github-apps.md) without giving Resource Plane/Job/API roles access to Tenant private keys.

## Work

- Resolve App/integration/approved binding from authoritative Tenant/Run context; validate active ownership, Installation/Repository integration, expected revision/generation. Never trust caller-supplied App ID or arbitrary Secret/Role.
- Implement trusted purpose-specific Secret provider/reader-role factory with server-approved Role ARN, exact Secret ARN and pinned version for owned mode. Keep shared AWSCURRENT compatibility.
- Add `GET /app` proof of App ID for draft verification. Separate external verification from DB transactions and recheck revision before accepting ready status.
- Partition bounded client/token/verifier pools by Tenant/integration/App/revision/generation; maintain read-only repository scope, pre-expiry refresh, serialized minting, secret-safe errors, and App-scoped cooldown.
- Revalidate generation on every use, invalidate stale pools on changes and fail closed. No shared/legacy fallback after owned failure.

## Acceptance / tests / docs

Progress: `TokenClient.VerifyApp` signs with the configured App key, calls GitHub `GET /app`, checks the returned App ID, and returns only fixed safe credential errors. `ResolveAppProofCredential` resolves only a draft Tenant-owned integration, exact approved binding, exact reader Role ARN, exact private-key Secret ARN and immutable 32-hex Version ID for the expected config revision and credential generation; it never returns OAuth/webhook refs. Missing pins, wildcard Role ARNs and `AWSCURRENT` are rejected. Integration tests cover cross-Tenant, stale revision/generation, malformed pin and disabled-state rejection. These are metadata/library primitives only: STS AssumeRole and Secrets Manager retrieval, no public proof endpoint, and no ready/active transition are enabled yet.

- Generated RSA keys, fake Secret/STS/GitHub providers test A/B isolation, correct Role/ARN/version, mismatched App proof, rotation, revoked/suspended configuration, concurrent requests and stale client/cache.
- Tokens/keys cannot appear in DB/messages/errors/artifacts; no public token endpoint is added.
- Document broker blast radius and exact IAM contract. Deployment/policy resources are Task 59; real secrets are not retrieved here.
