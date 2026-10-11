# Task 70: Tenant binding migration and cutover tooling

Status: Implemented locally; production migration/cutover remains approval-gated. Depends on: 67, 69.

## Scope

既存Installation bindingのdry-run/backfillと旧Run drain確認、ready→active/default選択の安全な手順を実装する。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- dry-runは変更なし。明示対象Tenantのみidempotentにbindingし、衝突を拒否する。
- 既存Run pinは変更せず、revision/generation照合とactivation条件を検証する。
- local rollback/再実行テストと運用手順を追加する。実cutoverは明示承認待ち。

## Handoff

- `000065_tenant_installation_app_identity.sql` adds a nullable App ID anchor. Setup callback stores only the App ID verified against the selected integration; historical rows without that identity are deliberately not inferred.
- Store/API/CLI implement read-only `migration-plan`, explicitly confirmed idempotent `migration-backfill`, expected revision+credential-generation fences, and ready→active/default selection after purpose-specific credential binding validation and all nonterminal Runs pinned to a different integration are drained. The previous active Tenant-owned default is disabled and its credential generation advanced atomically with the switch; previous shared registration is not modified.
- Backfill is scoped to explicit Tenant/integration/Installation IDs, checks the verified App ID and active unsuspended Tenant binding, refuses conflicts, applies all-or-nothing, records audit with the mutation, and never rewrites Workload Run pins/default selection.
- Tests cover dry-run immutability, idempotent rerun, App ID mismatch, unknown historical App identity, atomic mixed-batch rollback, stale revision/generation, legacy Run drain, activation, and preserved legacy Run pins. PostgreSQL/API integration test commands and final results are recorded in the handoff response.
- Related operator/API contract: [Tenant GitHub App design](../docs/tenant-github-apps.md), [API contract](../docs/api-contract.md).
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
