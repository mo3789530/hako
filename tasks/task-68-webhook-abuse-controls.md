# Task 68: Public Webhook abuse controls

Status: Todo (local implementation). Depends on: 58 の既存Webhook adapter.

## Scope

Secret取得前の安価な入力検証とintegration別のrate budgetを追加する。valid-shaped攻撃も対象とする。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- 不正ヘッダー・過大bodyでSTS/Secret呼び出しが発生しないことをcall-countテストする。
- 許容頻度超過はSecret取得前に拒否し、複数instance時の制約・429/retry方針を明記する。
- generation変更・Secret rotationを妨げず、shared経路の回帰とログのSecret非露出を検証する。

## Handoff

- 対象コード、テスト結果、関連docs、未検証の配備条件を記載する。
- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
