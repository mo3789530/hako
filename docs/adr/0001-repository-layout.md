# ADR 0001: リポジトリ構成

- Status: Accepted
- Date: 2026-09-23

## Context

HakoはControl Plane、Resource Plane、CLI、Gateway、Agentを持つ。初期からGo moduleやリポジトリを分割すると、共通型の変更と開発環境の整備が複雑になる。

## Decision

当面は単一Go moduleを使い、実行バイナリを`cmd/`、内部実装を`internal/`、設計・開発文書を`docs/`に置く。Control PlaneとResource Planeは別プロセス・別デプロイ単位として実装し、必要性が明確になった時点でGo module分割を再検討する。

## Consequences

- 初期開発で共通型・処理を再利用しやすい。
- `internal`の境界を守り、実装詳細を不用意に公開しない。
- 単一moduleであっても、Control PlaneとResource Planeの依存関係・IAM・デプロイを混同しない。
