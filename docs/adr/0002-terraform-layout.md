# ADR 0002: Terraformのstateとmodule境界

- Status: Accepted
- Date: 2026-09-23

## Context

Control PlaneとResource PlaneはAWS Account単位で分離する。単一stateに両方を含めると、権限境界、変更影響、運用担当を分離しにくい。

## Decision

再利用可能な定義は`infra/terraform/modules/`に置く。実環境のrootは`infra/terraform/environments/<environment>/<plane>/`に置き、Control Planeと各Resource PlaneでTerraform stateを分ける。state backendとアカウント固有値はAWSリソース実装時に環境ごとに設定する。

## Consequences

- Control PlaneとResource Planeを独立してplan/applyできる。
- CIでは認証情報を使わず、formatとbackendなしのvalidateを実行できる。
- 実環境へのapplyはstate backendとAWS認証方式を定義してから行う。
