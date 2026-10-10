# Task 58: Tenant App OAuth Setup and Webhook Routing

Status: In Progress (opaque route handler, webhook-only secret resolver boundary, post-HMAC Installation authorizer boundary, bounded previous-secret grace, and integration-scoped inbox identity implemented; production credential resolver/startup wiring and OAuth setup binding remain). Priority: P0.

## Goal

Route setup and signed events to the correct App without trusting query/payload Tenant claims, following [Tenant App design section 6](../docs/tenant-github-apps.md).

## Work

- Bind setup state to integration/config revision/credential generation and select slug/client ID/OAuth Secret from that trusted state. Preserve PKCE, one-time expiry, user-visible Installation verification, and final Owner/Admin recheck.
- Add opaque integration Webhook routes. Resolve only the candidate Secret, verify bounded raw-body HMAC before normalization/Tenant processing, then validate App/Installation identity. (Ingress/API boundary implemented; DB-backed resolver/authorizer and queued processor revalidation remain.)
- Change inbox dedup to integration + delivery ID and distinguish payload conflicts. Preserve shared legacy URL as shared-only alias; no global Secret probing or fallback.
- Handle pre-binding events without auto-activation, disabled route, old generation, suspended/revoked Installation and per-integration current/previous Webhook-secret grace.
- Keep public ingress credential access Webhook-purpose only; it must not load App private keys/OAuth Secrets.

## Acceptance / tests / docs

Progress: Added an opt-in `/v1/integrations/github/apps/:integration_id/webhook` HTTP route. The handler resolves only that opaque integration's webhook-purpose credential, verifies the raw-body HMAC before normalization or Tenant authorization, applies a time-bounded previous-secret grace window, checks integration generation/Installation through an injected authorizer, then writes an integration-tagged delivery. Inbox deduplication is `(integration_id, delivery_id)`; the legacy route stores under `shared`. No AWS secret values or Tenant claims are inferred from payload JSON. The route is not wired to an AWS-backed resolver in the default API process and remains unconfigured unless injected. OAuth setup still uses the shared verifier and is not Tenant App-ready.

- Unit tests cover invalid-signature ordering, route isolation, generation/Installation handoff, bounded previous-secret grace and error redaction. PostgreSQL integration tests prove same delivery ID is independent across integration routes while same-route replay/collision behavior is retained.
- Remaining: production Webhook-purpose reader implementation and IAM, persist/revalidate generation through queue processing, OAuth state pinning/integration-aware setup verifier, webhook inbox Tenant/App binding checks, and end-to-end rotation/replay tests.

- Fake GitHub/API/Postgres tests cover correct setup, cross-App callback, state changed mid-flight, membership removal, bad/missing signature, wrong route, replay/conflict, same delivery ID across integrations and bounded grace expiry.
- Existing shared setup/Webhook tests remain valid. Unverified payload cannot trigger Tenant Secret selection or state mutation.
- Update callback/log-redaction and App setup/runbook docs. No GitHub settings or real Secret modifications.
