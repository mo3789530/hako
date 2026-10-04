# Task 46: Hako Session Tickets and Gateway

Status: Blocked on Tasks 43 and 45. Priority: P1.

## Goal

Connect CLI/Browser to the correct Workspace through a zero-trust HTTPS/WSS proxy, without putting application data through the Control Plane.

## Work

- Issue short-lived, audience-bound Session Tickets only after Tenant/Workspace RBAC; bind user, Tenant, Workspace, Resource Plane, allowed channels, expiry, and unique ticket ID.
- Implement signature/key rotation, replay policy, expiry, revocation/authorization recheck, and audit-safe validation errors.
- Deploy a long-running ECS/Fargate Gateway with TLS/WSS, stream routing, rate/connection limits, and health/metrics.
- Keep TCP/22 and application/database ports private; Gateway is not an SSH bastion and must not trust caller-supplied routing IDs.

## Acceptance criteria

- Tests cover cross-Tenant/workspace rejection, expired/forged/replayed ticket, permission mismatch, routing spoof, rate limit, reconnect, and upstream failure.
- End-to-end stream tests cover shell/PTY and one forwarded localhost port without Control Plane data proxying.
- Terraform ingress is HTTPS/WSS 443 only with least-privilege Runtime connectivity; logs exclude ticket values and terminal contents unless explicitly audited policy allows.
- Document key rotation and incident revocation.
