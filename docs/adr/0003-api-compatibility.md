# ADR 0003: APIと非同期Operationの互換性

- Status: Accepted
- Date: 2026-09-23

## Context

CLI、Control Plane、Resource Planeは個別に更新される。HTTP APIとQueueメッセージの変更が同時リリースを要求すると、段階的デプロイとロールバックが難しくなる。

## Decision

外部HTTP APIは`/v1`のような明示的なmajor versionを持たせる。minorな追加変更は後方互換にし、既存フィールドの削除・意味変更は新majorで行う。Operationメッセージにはschema versionを含め、consumerは少なくとも現行と直前の互換versionを処理できるようにする。OpenAPIとメッセージschemaを契約の正本として管理する。

## Consequences

- API・message schemaの変更時に互換性レビューが必要になる。
- producerとconsumerの段階的デプロイが可能になる。
- schemaと実装の整合性をCIで検査する仕組みを後続タスクで追加する。
