# Task 60: Tenant GitHub App E2E and Rotation Runbook

Status: Blocked on Tasks 56–59. Priority: P0.

## Goal

Verify two independent Tenant Apps and the unchanged shared mode before permitting owned-mode rollout. This does not replace the full Software Factory execution E2E in Task 42.

## Work

- Add API/CLI + fake GitHub/Secrets/STS + PostgreSQL E2E for draft registration, proof, setup, repository binding, token scope, signed Webhook processing, disable and explicit shared/owned selection.
- Use distinct App IDs/RSA keys/OAuth secrets/Webhook secrets for A/B. Prove cross-Tenant/API/binding/route/cache/role attempts fail and never fallback.
- Test config changes during callback/mint, concurrent rotation, previous-secret grace expiry, stale replica generation, interrupted migration, in-flight Run handling and emergency revocation.
- Publish operator steps for provisioning, bounded rotation, all-replica invalidation, cleanup confirmation, safe audit and rollback. Keep actual App/Secret/AWS changes approval-gated.

## Acceptance / tests / docs

- Local E2E, regression, race and policy tests pass without public GitHub/real secrets. Existing always-running Postgres is not stopped.
- Docs distinguish per-Tenant IAM from shared broker process isolation and library completion from real deployment.
- Owned mode remains default-off; documented opt-in requires tested migration/IAM and an explicitly approved real-environment smoke. Do not mark runtime/Factory milestones complete here.
