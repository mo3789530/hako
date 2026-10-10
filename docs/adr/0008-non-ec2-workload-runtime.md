# ADR 0008: Lambda MicroVM Workload Runtime

- Status: Architecture direction accepted; provider API and PoC pending
- Date: 2026-10-10
- Scope: Software Factory Workload Runtime; aligned with Workspace Lambda MicroVM.

## Decision

ユーザー指定により実行基盤は **Lambda MicroVM** とする。先のFargate選定は撤回する。EC2 worker、AMI、EBS worker disk、EC2 lifecycle権限も追加しない。Gateway用ECS/Fargate案は今回のWorkload基盤訂正とは別の責務である。

Workspaceは論理的・永続的なリソース、MicroVMは交換可能なRuntime Allocationである。JobはRun attemptごとに独立したMicroVMへ配置し、実行済みAllocationを別Runへ再利用しない。

## 実装前に確認する事項

設計上のLambda MicroVMと通常のAWS Lambda function invocationを同一視しない。具体的な提供サービス/API、Region、MicroVMの作成・停止・再発見方法、権限をTask 61で特定する。現時点では提供APIや実行可能時間を検証済みとして扱わない。

- 既存Run APIの作成時から1秒〜24時間というdeadline契約との適合。
- Docker/containerd/Podman、compose、開発用DBに必要なguest権限と制限。先のnested-container除外は撤回し、Lambda MicroVMでの実証事項とする。
- MicroVMごとのTenant隔離、untrusted code、CPU/memory/disk制限、network default-deny。
- 固定commitのsource搬入、attempt限定artifact搬出、Secret非露出。
- 起動時間、quota、費用、cancel/launch競合、deadline、cleanup、障害復旧。
- Workspace向けsuspend/resume、永続volume、長時間接続はTask 43とも整合させる。

必要な能力が確認できない場合は実行機能をdefault-offのまま保ち、未達要件を記録する。別のcompute基盤へ自動的に変更しない。

## Runtime契約と責務

Workload adapterは `EnsureRunning`、`Cancel`、`GetStatus`、`CollectResult`、`Cleanup`を持つ。Tenant/Run/attempt/generation/placementを固定し、providerのidempotencyと再発見機構を確認してから実装する。cancel tombstone、stale result拒否、Run終端と物理cleanupの独立管理は既存の条件付きRP journalとCP result fencingを維持する。

- Control PlaneはDSQL、認可、placement、dispatch、result、deadlineの正本を持つ。
- Resource Planeはprovider操作、Runtime Allocation、command/result queue、artifact搬送、cleanupを所有する。DSQLへ直接接続しない。
- MicroVMのuntrusted codeへGitHub App private keyや広範なAWS資格情報を渡さない。source/artifactの権限は当該attemptへ限定する。
- SQS receiptは短時間のdurable handoff後に解放し、Run実行時間をqueue visibilityへ依存させない。
- ローカルPodmanは開発・command policy検証用であり、本番MicroVM隔離の実証は別途行う。

## Follow-up

Task 61でprovider/APIと実行契約を確認し、Task 39でadapter/controllerを接続する。Task 40の配備はその確認に依存する。実PoCで起動、container実行、隔離、停止、cleanup、artifact、長時間deadlineを検証するまでRun作成はdefault-off。

本ADRによるAWSリソース作成・変更はない。APIサーバーのLambda container image + Lambda Web Adapter配備は独立した設計であり、Workspace/JobのMicroVM lifecycleが実装済みであることを意味しない。
