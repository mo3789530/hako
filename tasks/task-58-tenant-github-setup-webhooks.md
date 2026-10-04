# Task 58: Tenant App OAuth Setup and Webhook Routing

Status: Blocked on Tasks 56 and 57. Priority: P0.

## Goal

Route setup and signed events to the correct App without trusting query/payload Tenant claims, following [Tenant App design section 6](../docs/tenant-github-apps.md).

## Work

- Bind setup state to integration/config revision/credential generation and select slug/client ID/OAuth Secret from that trusted state. Preserve PKCE, one-time expiry, user-visible Installation verification, and final Owner/Admin recheck.
- Add opaque integration Webhook routes. Resolve only the candidate Secret, verify bounded raw-body HMAC before normalization/Tenant processing, then validate App/Installation identity.
- Change inbox dedup to integration + delivery ID and distinguish payload conflicts. Preserve shared legacy URL as shared-only alias; no global Secret probing or fallback.
- Handle pre-binding events without auto-activation, disabled route, old generation, suspended/revoked Installation and per-integration current/previous Webhook-secret grace.
- Keep public ingress credential access Webhook-purpose only; it must not load App private keys/OAuth Secrets.

## Acceptance / tests / docs

- Fake GitHub/API/Postgres tests cover correct setup, cross-App callback, state changed mid-flight, membership removal, bad/missing signature, wrong route, replay/conflict, same delivery ID across integrations and bounded grace expiry.
- Existing shared setup/Webhook tests remain valid. Unverified payload cannot trigger Tenant Secret selection or state mutation.
- Update callback/log-redaction and App setup/runbook docs. No GitHub settings or real Secret modifications.
