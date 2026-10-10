# Task 58: Tenant App OAuth Setup and Webhook Routing

Status: In Progress (DB-backed Webhook-only credential resolver, exact-role/pinned-version secret reader, post-HMAC Installation authorizer, queued generation revalidation/quarantine, bounded previous-secret grace, integration-scoped inbox identity, opt-in API/Terraform route and IAM allowlist implemented; OAuth setup binding and real role provisioning remain). Priority: P0.

## Goal

Route setup and signed events to the correct App without trusting query/payload Tenant claims, following [Tenant App design section 6](../docs/tenant-github-apps.md).

## Work

- Bind setup state to integration/config revision/credential generation and select slug/client ID/OAuth Secret from that trusted state. Preserve PKCE, one-time expiry, user-visible Installation verification, and final Owner/Admin recheck.
- Add opaque integration Webhook routes. Resolve only the candidate Secret, verify bounded raw-body HMAC before normalization/Tenant processing, then validate App/Installation identity. (DB-backed metadata resolver, webhook-only STS reader and DB authorizer implemented; role provisioning/trust is not deployed.)
- Change inbox dedup to integration + delivery ID and distinguish payload conflicts. Preserve shared legacy URL as shared-only alias; no global Secret probing or fallback.
- Handle pre-binding events without auto-activation, disabled route, old generation, suspended/revoked Installation and per-integration current/previous Webhook-secret grace.
- Keep public ingress credential access Webhook-purpose only; it must not load App private keys/OAuth Secrets.

## Acceptance / tests / docs

Progress: Added an opt-in `/v1/integrations/github/apps/:integration_id/webhook` HTTP route. The handler rejects malformed/oversized transports before loading secrets, resolves only that opaque integration's webhook-purpose credential, verifies raw-body HMAC before normalization or Tenant authorization, applies a time-bounded previous-secret grace window, checks integration generation/Installation through a DB authorizer, then writes an integration-tagged delivery and pins the credential generation. The STS reader assumes only an approved exact Webhook Role and requests exact current/previous VersionIds under a Secret-ARN-limited session policy. The processor rechecks active integration generation and Installation binding before applying repository or lifecycle state; stale events are quarantined as failed. Inbox deduplication is `(integration_id, delivery_id)`; the legacy route stores under `shared`. Terraform exposes a default-off route and `sts:AssumeRole` allowlist restricted to Webhook-purpose role ARN paths. Real per-Tenant roles/trust policies and Secret versions are not provisioned. OAuth setup still uses the shared verifier and is not Tenant App-ready.

- Unit tests cover invalid-signature ordering, route isolation, generation/Installation handoff, bounded previous-secret grace and error redaction. PostgreSQL integration tests prove same delivery ID is independent across integration routes while same-route replay/collision behavior is retained.
- Remaining: provision and review per-Tenant role trust/Secret version metadata, OAuth state pinning/integration-aware setup verifier, and end-to-end rotation/replay tests. Tenant-owned mode remains default-off.

- Fake GitHub/API/Postgres tests cover correct setup, cross-App callback, state changed mid-flight, membership removal, bad/missing signature, wrong route, replay/conflict, same delivery ID across integrations and bounded grace expiry.
- Existing shared setup/Webhook tests remain valid. Unverified payload cannot trigger Tenant Secret selection or state mutation.
- Update callback/log-redaction and App setup/runbook docs. No GitHub settings or real Secret modifications.
