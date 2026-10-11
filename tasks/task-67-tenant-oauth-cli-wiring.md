# Task 67: Tenant OAuth API/CLI wiring

Status: Implemented locally (API mTLS resolver config, CLI setup command and real mTLS broker→API→PostgreSQL callback E2E with fake GitHub/Secret providers pass). Production broker URL/certificate distribution, private networking and deployment operations remain. Depends on: 66, 72.

## Scope

Task 58のadapterをproduction API設定へ接続し、Tenant-owned App setupをCLIから開始できるようにする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- default-offとshared互換を維持し、broker未設定時はfail closed。
- CLI/API/PostgresでOwner/Admin成功、一般member拒否、別Tenant拒否、callback pinを検証する。
- 設定・エラー・setup手順を文書化し、実GitHub設定変更やactivationは行わない。

## Handoff

- API startup accepts the broker URL, client certificate/key paths and CA bundle as an all-or-none configuration. Tenant setup is exposed only when the existing App management feature flag is enabled. Missing broker config keeps the owned endpoint disabled.
- Added `hako github app setup --tenant ... --integration ...`, which calls the authenticated API, opens the short-lived returned setup URL and prints the response. API client validates the returned GitHub URL and expiry.
- Setup URL and OAuth authorization redirect are restricted to exact `https://github.com` host/path families. The mTLS client refuses redirects. Configured broker URLs are validated at process startup; partial config or enabling broker config without App management fails startup.
- Unit and existing PostgreSQL integration suites pass. Existing Tenant callback integration uses an injected fake resolver; a complete DB callback through the real mTLS adapter is still pending.
- Added an isolated PostgreSQL integration that connects API→HTTP mTLS broker→live setup pin resolver, invokes setup URL, PKCE authorization redirect and Installation verification, then confirms the binding persisted. GitHub verifier and OAuth Secret reader are fakes; no real Secret/GitHub account is used.
- Remaining: production broker endpoint/certificate distribution, private networking and operational certificate rotation/deployment. No external App settings were changed.

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
