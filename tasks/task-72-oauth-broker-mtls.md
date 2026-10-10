# Task 72: OAuth Broker Mutual TLS Transport

Status: In Progress (HTTPS endpoints, mTLS config, API adapter and handler identity tests implemented; certificate issuance/rotation and production composition remain). Priority: P0. Parent: Task 66.

## Goal

Connect the Control Plane OAuth setup adapter to the credential broker over authenticated internal HTTPS. Only the designated Control Plane workload identity may invoke setup URL and Installation verification operations.

## Work

- Require TLS 1.3 and a client certificate issued by the broker trust root; validate the expected Control Plane URI SAN and reject missing/other workload identities before resolving Tenant credentials.
- Propagate the verified caller identity into broker authorization policy. Tenant, integration, App, revision and credential generation must still be resolved and checked against live database state.
- Bound request/response sizes and timeouts, use no-store responses, redact errors, and expose only setup URLs or verified Installation identity.
- Provide an API-side `SetupVerifier` adapter configured with a client certificate. Keep broker endpoint private and do not wire the public API handler directly to Secret access.

## Acceptance criteria

- TLS integration tests prove missing/untrusted/wrong-SAN client certificates fail before resolver or Secret reader invocation.
- Authorized Control Plane requests succeed only for an active matching pin; changed Tenant, App, revision, generation or revoked configuration fails closed.
- Request size, deadlines, response status, callback state/PKCE, App identity, and error redaction have tests.
- Document certificate provisioning/rotation and broker/API IAM/network boundaries. Do not provision certificates or apply AWS changes in this task.

## Progress

Added `/v1/oauth/setup-url`, `/authorize-url`, and `/verify` handlers; server TLS config requires TLS 1.3 and verified client certificates, and the handler checks the Control Plane URI SAN. Added an API-side mTLS client constructor and `TenantGitHubSetupVerifierResolver` adapter. The verified identity is passed through request context to the broker authorization callback. API now returns a sanitized unavailable response if broker URL calls fail. Handler tests cover absent/unverified/wrong identity and prove credential resolution is not called. Remaining work includes full TLS handshake tests, certificate lifecycle, real resolver/authorizer composition, and production API wiring; no certificate provisioning or AWS changes were made.
