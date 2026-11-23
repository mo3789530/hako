# Hako 実装状況のまとめ

更新日: 2026-11-23

このページは、現在の設計・実装・受入状況へ入るための案内です。各機能の詳細仕様とタスク台帳を正とし、このページでは全体像と残る大きな受入ゲートをまとめます。

## 製品の実行先

| 機能 | 実行先 | 実装状況 |
| --- | --- | --- |
| GitHub Actions / 承認済み Claude Code Action | Resource Plane の Lambda MicroVM | ローカルでAdmission、Runner起動、結果、cleanupまでfake/PostgreSQLで検証済み。実AWSのイメージ、IAM、ネットワーク、GitHub連携は未受入。 |
| Hako Coding Agent | Amazon Bedrock AgentCore Runtime | attempt固定、source/proposal契約、Worker、AWS SDK adapter、専用SQS queue、default-off Lambda consumerとローカルlease heartbeatを実装。Runtime image、独立検証接続、実AWS隔離・cleanupは未受入。 |
| 管理Workspace / VS Code | Resource Plane の Lambda MicroVM | API、Gateway、Guest、永続ストレージ等を実装。実AWS MicroVM上のWorkspace受入は未実施。 |
| ローカルWorkspace | Firecracker + KVM | AWSなしで認証API、Control Plane、Outbox、Resource Controller、Gateway、Guestを接続。実Guestでcreate/delete、CLI exec、PTY shell、upload/download、repository初回配置・同一archive retry・編集保持、cold-resume、VS Code描画、SSHを確認。 |

## 設計上の境界

- Control PlaneはTenant認可、Workspace/Run状態、attempt識別、結果公開可否を保持します。
- Resource Planeはruntime、queue、IAM、Guest lifecycleを実行します。queue messageやrepository内容からprovider/role/sessionを選ばせません。
- RepositoryとWorkflowのコードは隔離対象です。GitHub書込み資格情報や広いAWS権限をGuest/AgentCore sessionへ渡しません。
- Coding Agentの提案は未検証データです。Hakoがattempt/sourceに結び付け、独立検証が成功した後にだけ公開可能になります。
- Local Workspaceとmanaged Lambda MicroVMは別の運用プロファイルです。Local profileはAWS credentialなしで起動できます。

## 仕様書の読み方

1. [実行アーキテクチャ](execution-architecture.md) — 機能ごとの実行先、信頼境界、lifecycle invariant。
2. [Software Factory](software-factory.md) — GitHub起点のJob/Agent機能と全体フロー。
3. [Coding Agent platform](coding-agent-platform.md) — AgentCoreの役割と実行フロー。
4. [AgentCore runtime placement](agentcore-runtime-placement.md) — Runtime/session、request/result、cleanup、deployment設計。
5. [Coding Agent source/proposal contract](coding-agent-source-proposal-contract.md) と [verification policy](coding-agent-verification-policy.md) — immutable sourceと独立検証。
6. [Workspace Lambda MicroVM](workspace-lambda-microvm.md)、[CLI](workspace-cli.md)、[Gateway](workspace-gateway.md) — managed WorkspaceのAPIと接続。
7. [Local Workspace MicroVM runbook](local-workspace-microvm.md) — KVM上でAWSなしに起動・検証する手順。
8. [タスク台帳](../tasks/README.md) — 未完了作業、依存関係、検証結果、外部受入ゲート。

## 直近の完了・確認

- 2026-11-23: authenticated local CLIでlogin/create/running/delete/deletedを実KVM Guestに対して確認。実Firecracker Guestへの署名付きGateway stream smokeでexec、PTY shell、upload/download、Chromium IDE表示、SSH公開鍵認証後のGuest commandを受入。
- 2026-11-23: 実Firecracker Guestを使うopt-in Go smokeを追加し、Hako CLIの`exec`がCLI session発行・失効、署名付きGateway ticket、host-private vsockを通してUID 100000のGuest commandを実行することを確認。`install_repository`の初回配置、同一archive retry、編集後の再送拒否と編集保持もGuest/Gateway経由で確認。再実行は`make test-local-workspace-cli-firecracker`。対話shell CLI全体の受入は残る。
- HTTP Request ContextをWebSocket upgrade後の処理に渡していた問題を修正し、exec、PTY shell、upload/downloadが実Guestで成功。Guest停止はidentity-boundなvsock controlでPID 1に依頼し、Agent停止とext4 unmountを待つ。cold-resume後も同一diskのファイルを再取得してbyte一致を確認。Gateway経由のChromium IDE描画、SSH公開鍵認証、Controller restart/retry/deleteも実KVMで受入。
- 2026-11-23: repository受入の最初の試行はGuestの`/tmp`が`0755`でUID 100000から書き込めず失敗。initrd packagerが`/tmp`を`01777`へ固定するよう修正し、newc mode回帰テストと、現行Agent/Guest-initを含むinitrdでの実KVM初回checkout/readback/retry/edit-preservation smokeが成功。
- AgentCore command queue/DLQはdefault-off。Dispatcher roleだけがcommand queueへ送信できる。Worker Lambda/event source/IAMは任意の明示設定が必要。
- AgentCore Worker ARM64 ZIPのローカルbuildとResource Plane module/dev Terraform validateは成功。長時間処理中のattempt lease heartbeatはfake testで確認。AWS deployやAPI呼び出しはしていない。
- Goのローカルunit/integration、race、Terraform mock-plan、Python unitの結果は各Taskに記録。ローカルfakeはAWS隔離や実ネットワークを証明しません。

## 大きな残件

1. **Local Workspace対話受入** — 実GuestでCLI `exec`、PTY shell、upload/download、repository初回配置と編集保持、同一disk cold-resume、VS Code描画、SSH、Controller restart/retry/deleteを確認。対話shell CLIの操作全体とAWS managed runtime acceptanceが残る。詳細は[Task 98](../tasks/task-98-local-workspace-microvm.md)。
2. **AgentCore実行完成** — Kiriでのdigest-pinned ARM64 Runtime image作成、IAM/egress/cost/quota検証を行う。独立Verifierは既存Workload result/CP queueにproposalを渡す共通経路を使用し、local boundary testsがある。lease renewalはローカル実装済みで、実AWS上の挙動は未受入。詳細は[Task 90](../tasks/task-90-agentcore-coding-agent.md)、[Task 92](../tasks/task-92-coding-agent-harness-model.md)、[Task 94](../tasks/task-94-coding-agent-verification-terraform.md)、[Task 95](../tasks/task-95-agent-isolation-and-credentials.md)、[Task 96](../tasks/task-96-agentcore-lifecycle-operations.md)。
3. **Managed runtime AWS acceptance** — WorkspaceとRunnerのMicroVM、AgentCore、IAM、ネットワーク、artifact/cleanupを承認済みstagingで検証。詳細は[Task 99](../tasks/task-99-managed-runtime-acceptance.md)および各deployment task。
4. **GitHub/AWSの実連携受入** — 実installation、証明書/KMS、cross-account IAM、Webhooks/Checks、migrationをstagingで確認。taskごとに必要なGitHub/AWS設定がある。

AWS資格情報、アカウント設定、GitHub App、実行承認がない受入項目はローカル作業で完了扱いにしません。現状の個別ステータスはタスク台帳を参照してください。
