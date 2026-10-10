# Task 61: Validate Lambda MicroVM Runtime Contract

Status: In progress (architecture fixed; provider API and PoC pending). Priority: P0. Depends on: Task 39 substrate-neutral foundations; coordinate with Task 43.

## Goal

実行基盤はユーザー指定のLambda MicroVM。具体的provider/APIとWorkload要件の適合を確認する。[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。先のFargate選定は撤回済み。

## Work

- [x] Lambda MicroVM前提にADR・README・Task 40を訂正する。EC2 workerとFargate Workload workerは追加しない。
- [ ] 具体的な提供サービス/API、Region、作成/停止/再発見/cleanup、認証・IAMを特定する。通常のLambda invocationを永続MicroVM APIと仮定しない。
- [ ] 1秒〜24時間のRun deadline、Docker/containerd/Podman、compose、Tenant隔離、untrusted code、network制限、source/artifact搬送を確認する。
- [ ] `EnsureRunning`/`Cancel`/`GetStatus`/`CollectResult`/`Cleanup`のprovider mapping、冪等性、cancel競合、retryと障害復旧を定義する。
- [ ] cold start、quota、費用、container実行、停止/cleanupのPoCを記録する。Workspace向け能力はTask 43と共有する。

## Acceptance criteria

- provider APIと実装対象が具体的に記録され、Task 40が配備/IAMを設計できる。
- 未達要件は明示する。基盤を自動変更せず、未検証の24時間実行やnested container対応を保証しない。
- CP/RP分離、DSQLへのRP直接接続禁止、Secret非露出、attempt fencingを維持する。
- 実行pathと監視が検証されるまでRun作成はdefault-off。AWS applyは含まない。
