# Task 70: Tenant binding migration and cutover tooling

Status: Todo (local implementation). Depends on: 67, 69.

## Scope

既存Installation bindingのdry-run/backfillと旧Run drain確認、ready→active/default選択の安全な手順を実装する。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- dry-runは変更なし。明示対象Tenantのみidempotentにbindingし、衝突を拒否する。
- 既存Run pinは変更せず、revision/generation照合とactivation条件を検証する。
- local rollback/再実行テストと運用手順を追加する。実cutoverは明示承認待ち。

## Handoff

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
