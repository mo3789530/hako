# Task 68: Public Webhook abuse controls

Status: Implemented locally (header preflight, DSQL shared per-integration counter, 429 path and API Gateway route throttle; unit/PostgreSQL integration tests and Terraform validation pass). Alerting, deployment review and live AWS configuration remain default-off/unapplied. Depends on: 58 の既存Webhook adapter.

## Scope

Secret取得前の安価な入力検証とintegration別のrate budgetを追加する。valid-shaped攻撃も対象とする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- 不正ヘッダー・過大bodyでSTS/Secret呼び出しが発生しないことをcall-countテストする。
- 許容頻度超過はSecret取得前に拒否し、複数instance時の制約・429/retry方針を明記する。
- generation変更・Secret rotationを妨げず、shared経路の回帰とログのSecret非露出を検証する。

## Handoff

- Tenant webhook preflight checks unique/syntactically valid event, UUID delivery ID, and SHA-256 HMAC header before reading the bounded body or calling the resolver. Invalid media type/header/oversized bodies never reach STS/Secrets Manager.
- Added migration 000064 with one fixed-window counter row per integration. The DSQL/PostgreSQL UPSERT only inserts for active integrations, atomically increments across API instances, resets after 60 seconds, and creates no row for unknown/disabled routes. Exceeded budgets return 429 with `Retry-After: 60`; DB errors fail closed with 503. API default is 120 attempts/minute, tunable through `HAKO_TENANT_GITHUB_WEBHOOK_RATE_LIMIT` (1..100000).
- API Gateway route throttle is configured at 50 requests/second and burst 100 by default, with bounded Terraform overrides. This is a coarse ingress cap shared by all Tenant routes; the database counter is per integration.
- Unit tests cover malformed/duplicate headers and prove rate denial precedes credential resolution. PostgreSQL integration proves budget, expiry reset and no random-route row growth.
- Remaining: alerting/metrics for 429 and DSQL failures, deployment review and eventual approved rollout. No AWS apply was run.

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
