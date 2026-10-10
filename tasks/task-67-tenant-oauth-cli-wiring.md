# Task 67: Tenant OAuth API/CLI wiring

Status: Todo (local implementation). Depends on: 66.

## Scope

Task 58のadapterをproduction API設定へ接続し、Tenant-owned App setupをCLIから開始できるようにする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- default-offとshared互換を維持し、broker未設定時はfail closed。
- CLI/API/PostgresでOwner/Admin成功、一般member拒否、別Tenant拒否、callback pinを検証する。
- 設定・エラー・setup手順を文書化し、実GitHub設定変更やactivationは行わない。

## Handoff

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
