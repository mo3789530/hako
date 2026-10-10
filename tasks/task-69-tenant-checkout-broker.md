# Task 69: Tenant proof/checkout broker wiring

Status: Todo (local implementation). Depends on: 57, 38.

## Scope

Runのimmutable credential pinからtrusted brokerを呼び、Repository限定tokenと固定SHA checkoutを接続する。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- API/RPへApp private keyを渡さず、broker内部要求を認証する。
- Tenant/App/Installation/Repository/generation mismatchとdisable時に拒否し、shared fallbackしない。
- fake broker/GitHubとPostgresでrotation、失敗retry、既存shared Run互換をテストする。

## Handoff

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
