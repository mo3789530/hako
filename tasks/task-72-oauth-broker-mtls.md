# Task 72: OAuth Broker Mutual TLS Transport

Status: Implemented locally (HTTPS endpoints, TLS 1.3 mTLS server/client, API adapters, full HTTPS handshake tests and executable TLS config implemented; certificate issuance/rotation and production network remain). Priority: P0. Parent: Task 66.

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

Added `/v1/oauth/setup-url`, `/authorize-url`, `/verify`, and `/v1/checkout/token` handlers. Server TLS config requires TLS 1.3 and verified client certificates; handlers require the exact Control Plane URI SAN before resolving credentials. API-side mTLS clients refuse redirects and enforce bounded timeouts. Real TLS handshake tests cover client certificate trust, identity, OAuth setup and checkout token request. `cmd/hako-github-broker` validates all-or-none server cert/key/client-CA config. See [Internal GitHub Credential Broker](../docs/github-credential-broker.md). Remaining work is certificate issuance/rotation, revocation/monitoring, private production network and deployment. No certificate provisioning or AWS changes were made.
