# Task 71: Two-Tenant App local E2E

Status: Implemented locally; production rollout remains unverified. Depends on: 67–70.

## Scope

Task 60の横断受入テストをfake外部API・常時起動Postgresで実行可能にする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- 二Tenantでsetup→Webhook→Run→checkoutを検証し、別Tenant/別App/replayを拒否する。
- rotation・disable・移行・shared回帰・並行callbackとDB障害retryを検証する。
- CI実行方法と証跡を記載する。local合格だけでAWS配備完了にしない。

## Handoff

- `internal/integration/tenant_github_app_e2e_integration_test.go` combines concurrent verified setup callbacks for two Tenants, per-integration HMAC webhook ingress/inbox, repository sync, server-pinned Workload Run creation, and persisted Run checkout credential resolution.
- Negative assertions cover wrong App identity, duplicate delivery replay, same delivery ID with changed payload, cross-Tenant checkout, rotated generation, and disabled integration. A separate legacy shared-App Run proves the shared path remains unpinned. Setup transaction uses a synthetic retryable DSQL serialization conflict on its first `Begin` and verifies the retry succeeds.
- Task 70 PostgreSQL tests cover migration dry-run/no writes, explicit App-ID identity, conflict rollback, idempotent backfill, unknown historical identity, revision/generation fence, legacy Run drain, activation, and existing Run pin immutability. Task 67 covers the API→mTLS broker→PostgreSQL OAuth callback; Task 69 covers checkout broker mTLS/token-mint boundary.
- Run locally with `make test-integration-local` (or the CI `make test-integration` target with `HAKO_TEST_DATABASE_URL`). Tests create isolated PostgreSQL schemas and never stop the shared database. CI runs the full integration suite in `.github/workflows/ci.yml`.
- The E2E uses fake secrets/provider identity and does not read real Secrets or call GitHub/AWS. Production certificates, IAM trust, AWS Lambda MicroVM/image/network behavior, worker deployment, and actual Tenant cutover remain unverified.
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
