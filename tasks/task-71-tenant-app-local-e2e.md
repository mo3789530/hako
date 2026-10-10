# Task 71: Two-Tenant App local E2E

Status: Todo (local implementation). Depends on: 67–70.

## Scope

Task 60の横断受入テストをfake外部API・常時起動Postgresで実行可能にする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- 二Tenantでsetup→Webhook→Run→checkoutを検証し、別Tenant/別App/replayを拒否する。
- rotation・disable・移行・shared回帰・並行callbackとDB障害retryを検証する。
- CI実行方法と証跡を記載する。local合格だけでAWS配備完了にしない。

## Handoff

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
