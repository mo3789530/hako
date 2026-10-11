# Task 66: Tenant OAuth-purpose credential broker

Status: Implemented locally (OAuth-purpose DB resolver, exact-role/version Secret reader, OAuth/checkout HTTP service composition and mTLS server executable implemented). Production certificate lifecycle, IAM provisioning and deployment remain. Depends on: Task 58の既存state pin/callback adapter（親58全体の完了は不要）。

## Scope

承認済みOAuth-purpose Role/Secret Versionだけを使うtrusted brokerを実装する。APIへSecretを返さず、setup pinと認証済み内部要求からOAuth検証を行う。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- Tenant/integration/revision/generation一致を要求し、private-key/Webhook-purpose Roleを拒否する。
- fake STS/Secrets/GitHubで越境・rotation・broker認証失敗・Secret非露出をテストする。
- 実AWS Secret取得やRole配備は行わず、broker transportとIAM境界をドキュメント化する。

## Handoff

- Migration 000063 adds nullable OAuth reader Role/version metadata. Missing provisioning fails closed; existing shared setup is unchanged.
- `ResolveSetupCredential` revalidates Tenant/App/revision/generation, selects only approved OAuth-purpose metadata, rejects Role/Secret reuse across purposes, and redacts formatting/JSON. PostgreSQL integration tests cover unprovisioned access and mismatched pins.
- OAuth reader: `TenantOAuthSecretReader` uses the approved exact Role, exact Secret ARN session policy, 900-second STS session and immutable VersionId with a 10-second read timeout. No global credential mutation or secret cache. Errors and access metadata formatting/JSON are redacted. Fake tests cover success, STS failure/expiry, nil client, wrong version, empty Secret and provider errors.
- Broker core: `internal/githubbroker.OAuthService` requires injected caller authorization, resolves the live pin for every operation, reads secrets only for verification, validates returned App/Installation identity, and rechecks authorization and credential metadata after remote I/O. URL generation reads no Secret. Fake tests cover unauthenticated/cross-Tenant requests, rotation, revocation, wrong App and sanitized remote failures.
- Transport progress is recorded in [Task 72](task-72-oauth-broker-mtls.md). The server requires verified TLS client certificates and exact Control Plane URI SAN; production TLS configuration and certificate lifecycle still need review/tests.
- Full fake-provider real-adapter API→mTLS broker→PostgreSQL callback E2E now passes under Task 67. Remaining: certificate/role provisioning and deployment review. No real Secret was retrieved.
- `cmd/hako-github-broker` now composes OAuth-purpose secret access, DB-backed Run/pin checkout resolution, token issuance, and a TLS 1.3 mTLS listener. Environment and non-deployment boundaries are in [Internal GitHub Credential Broker](../docs/github-credential-broker.md). The process was not deployed and no real Secret was retrieved.

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
