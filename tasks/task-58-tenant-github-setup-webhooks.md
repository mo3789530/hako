# Task 58: Tenant App OAuth Setup and Webhook Routing

残作業の実行単位：[Task 66](task-66-tenant-oauth-broker.md)、[Task 67](task-67-tenant-oauth-cli-wiring.md)、[Task 68](task-68-webhook-abuse-controls.md)。親タスクは集約用に保持し、子タスク完了だけで配備済みとはしない。

Status: Local implementation and acceptance complete through Tasks 66–72 and 78–79. Tenant Webhook ingress/authorization, pinned OAuth setup/callback, API/CLI mTLS wiring, checkout broker, abuse controls, and two-Tenant lifecycle E2E are implemented locally. Production IAM/Role/Secret provisioning, certificate lifecycle, network review, and deployment remain approval-gated. Priority: P0.

## Goal

Route setup and signed events to the correct App without trusting query/payload Tenant claims, following [Tenant App design section 6](../docs/tenant-github-apps.md).

## Work

- Bind setup state to integration/config revision/credential generation and select slug/client ID/OAuth Secret from that trusted state. Preserve PKCE, one-time expiry, user-visible Installation verification, and final Owner/Admin recheck.
- Add opaque integration Webhook routes. Resolve only the candidate Secret, verify bounded raw-body HMAC before normalization/Tenant processing, then validate App/Installation identity. (DB-backed metadata resolver, webhook-only STS reader and DB authorizer implemented; role provisioning/trust is not deployed.)
- Change inbox dedup to integration + delivery ID and distinguish payload conflicts. Preserve shared legacy URL as shared-only alias; no global Secret probing or fallback.
- Handle pre-binding events without auto-activation, disabled route, old generation, suspended/revoked Installation and per-integration current/previous Webhook-secret grace.
- Keep public ingress credential access Webhook-purpose only; it must not load App private keys/OAuth Secrets.

## Acceptance / tests / docs

Progress: Added an opt-in `/v1/integrations/github/apps/:integration_id/webhook` HTTP route. The handler rejects malformed/oversized transports before loading secrets, resolves only that opaque integration's webhook-purpose credential, verifies raw-body HMAC before normalization or Tenant authorization, applies a time-bounded previous-secret grace window, checks integration generation/Installation through a DB authorizer, then writes an integration-tagged delivery and pins the credential generation. The STS reader assumes only an approved exact Webhook Role and requests exact current/previous VersionIds under a Secret-ARN-limited session policy. The processor rechecks active integration generation and Installation binding before applying repository or lifecycle state; stale events are quarantined as failed. Inbox deduplication is `(integration_id, delivery_id)`; the legacy route stores under `shared`. Terraform exposes a default-off route and `sts:AssumeRole` allowlist restricted to Webhook-purpose role ARN paths. Real per-Tenant roles/trust policies and Secret versions are not provisioned. OAuth-purpose resolver, broker, API mTLS configuration, and CLI setup were subsequently implemented in Tasks 66/67/72; owned setup remains disabled unless the broker configuration and App-management feature flag are enabled.

- Unit tests cover invalid-signature ordering, route isolation, generation/Installation handoff, bounded previous-secret grace and error redaction. PostgreSQL integration tests prove same delivery ID is independent across integration routes while same-route replay/collision behavior is retained.
- OAuth adapter progress: migration 000062 and the store pin Tenant/integration/App/revision/generation into one-time state. An injected trusted resolver enables the owned setup API and state-selected callback, rejects wrong-App results and changes during verification, rechecks final RBAC, and inserts an immutable Installation binding. Shared callbacks cannot replace owned bindings. API/Postgres tests cover these paths. The production API binary does not yet configure this resolver.
- Remaining: provision and review per-Tenant role trust/Secret version metadata, and complete production certificate/network/deployment review. Tenant-owned mode remains default-off. Local OAuth API/CLI/broker integration is tracked in Tasks 66/67/72; local Tenant Webhook adapter composition is covered by `TestTenantWebhookCredentialResolverUsesPinnedTenantWebhookSecretOnly` using synthetic STS/Secrets Manager endpoints.

App identity follow-up: optional `installation.app_id` is preserved in the normalized event and must match the route's registered App at ingress and processor authorization. Invalid IDs are rejected during normalization. Unit and PostgreSQL tests cover a different App's pre-binding event, wrong-App queue quarantine without repository mutation, and database lookup failures remaining retryable.

- Fake GitHub/API/Postgres tests cover correct setup, cross-App callback, state changed mid-flight, membership removal, bad/missing signature, wrong route, replay/conflict, same delivery ID across integrations and bounded grace expiry.
- Existing shared setup/Webhook tests remain valid. Unverified payload cannot trigger Tenant Secret selection or state mutation.
- Update callback/log-redaction and App setup/runbook docs. No GitHub settings or real Secret modifications.

## Local acceptance refresh (2026-10-25)

The prior “Remaining” list above described work later completed by Tasks 66–72
and Task 71. Re-run the dependency-ordered local acceptance with
`make test-tenant-github-apps-integration`. It covers the two-Tenant
setup→Webhook→Run→checkout E2E, API OAuth callback through the mTLS Broker and
PostgreSQL, exact App proof credential pin resolution, checkout credential
resolution, and Tenant webhook integration cases. All external GitHub/Secret providers in
these tests are fake. Production trust policies, certificates, network access,
and deployment remain unverified and have not been applied.
