# Hako Implementation Task Board

このディレクトリは、次の実装担当（Lunnaを含む）が一つずつ着手できる粒度にしたタスクの実行台帳です。設計上のSystem of Recordは引き続き[README](../README.md)、コンポーネント仕様は各`docs/`です。ここでは未完了項目を依存関係・境界・受入条件に整理します。

## 着手ルール

1. `P0`から順に、依存タスクが完了しているものを一件だけ選ぶ。
2. 着手前に`git status`、対象コード、タスク記載の仕様を確認する。既存変更と常時起動のPostgresを保持する。
3. タスクの受入条件を実装・テストし、重要な仕様変更は関連`docs/`とREADMEの状態記述へ反映する。
4. AWS `apply`、実AWSリソース削除、秘密情報の取得/表示は別途明示承認がない限り行わない。Podmanを使うローカルテストは許可されているが、DBコンテナを停止しない。
5. 終了時に検証結果・未完了項目・commit hashを報告する。別タスクへ範囲を広げず、次の依存可能タスクを示す。

## 優先順位と依存関係

### P0 — Software Factoryの安全な最小実行経路

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 1 | [36 — Worker実行基盤ADR](task-36-workload-worker-adr.md) | 完了（設計のみ）：使い捨てEC2、短時間handoff、fencing。実装は39/40 | なし |
| 2 | [37 — GitHub App資格情報](task-37-github-app-credentials.md) | 完了（library）：AWSCURRENT、JWT、Repository限定read-only token。配備は40 | なし |
| 3 | [38 — 固定commit checkout](task-38-workload-source-checkout.md) | 完了：Tenant/repository認可後に固定SHAを安全にcheckout、coverage 83.6% | 37（完了） |
| 4 | [39 — Workload実行handler](task-39-workload-execution-handler.md) | 進行中：DBからの実行情報解決とResource Planeへの耐久dispatch handoff | 36, 38 |
| 5 | [40 — Worker/IAM/Artifact配備](task-40-workload-worker-deployment.md) | 選定基盤へworker、Queue、S3/IAM/監視を配備 | 36, 39 |
| 6 | [41 — GitHub Checks](task-41-github-checks.md) | queued/in-progress/completed結果とcommit照合 | 37, 39 |
| 7 | [51 — Webhookイベント運用](task-51-webhook-events.md) | PR等のRepository Event正規化、Delivery監査、retention/replay | 37, 39 |
| 8 | [42 — Factory end-to-end](task-42-software-factory-e2e.md) | Installからisolated test、Check、log、cleanupを検証 | 38–41, 51 |

### P0追加 — Tenant別GitHub App（今回の要望で優先）

設計は[ADR 0007](../docs/adr/0007-tenant-github-apps.md)と[Tenant App仕様](../docs/tenant-github-apps.md)。Tenant-ownedのactivationは後続の資格情報検証・Webhook・移行が終わるまでdefault-off。Task 56のmetadata/API基盤は完了し、次はTask 57。

| Task | 内容 | Depends on |
|---|---|---|
| [56 — Tenant App Registry/API](task-56-tenant-github-app-registry.md) | 完了：metadata、Owner/Admin API/CLI、承認binding、inert draft | 37（完了） |
| [57 — Tenant資格情報resolver](task-57-tenant-github-credential-resolver.md) | App proof、Role/Secret/version解決、cache/generation分離 | 56 |
| [58 — Tenant OAuth/Webhook](task-58-tenant-github-setup-webhooks.md) | state固定、opaque ingress、HMAC後のTenant routing | 56, 57 |
| [59 — 移行/IAM](task-59-tenant-github-migration-iam.md) | shared backfill、Run固定、exact ARN Role、drain/rollback | 56–58 |
| [60 — Tenant App E2E/運用](task-60-tenant-github-app-e2e.md) | 二Tenant越境拒否、rotation、migration、shared回帰 | 56–59 |

### P1 — Workspaceの実AWS Runtimeと永続データ

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 9 | [43 — Workspace Runtime実装](task-43-workspace-runtime.md) | ADR、durable fencing、MicroVM lifecycle、補償cleanup | なし |
| 10 | [44 — EFS Workspace保存](task-44-efs-workspace.md) | Access Point、mount、再作成時のデータ維持 | 43 |
| 11 | [45 — hako-agent](task-45-hako-agent.md) | 起動、heartbeat、exec、PTY、process管理、Secret参照 | 43 |
| 12 | [46 — Session/Gateway](task-46-session-gateway.md) | 短命ticket、ECS Gateway、HTTPS/WSS proxy | 43, 45 |
| 13 | [47 — 開発者CLI/Service Catalog](task-47-developer-ux.md) | shell/forward/IDE、Postgres/Redis catalog | 44–46 |

### P2 — Placementの自動運用、AWS検証、運用準備

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 14 | [48 — Health reporterとplacement運用](task-48-health-reporter.md) | 認証付きreporter、AWS probes、freshness、容量/health検証 | なし |
| 15 | [49 — Terraform/AWS E2E](task-49-aws-environment-e2e.md) | Account分離環境、実AWS apply手順、作成〜接続〜cleanup検証 | 43–46, 48 |
| 16 | [50 — Security/Operations](task-50-security-operations.md) | 監査、アラーム、backup/recovery、孤立リソース、CI/AWS定期E2E | 40, 42, 49 |

### P3 — Software Factoryの拡張と利用制御

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 17 | [52 — GitHub Actions Runner Mode](task-52-ephemeral-actions-runner.md) | workflow_jobからephemeral runner起動・deregister | 40, 41, 51 |
| 18 | [53 — Native Pipeline/Agent](task-53-native-pipeline-agent.md) | strict factory schema、Issue→Agent→PR | 39, 50, 51 |
| 19 | [54 — Preview/Catalog/Deploy](task-54-preview-catalog-deploy.md) | Preview、Software Catalog、OIDC deploy policy | 43–46, 50, 51 |
| 20 | [55 — Quota/Usage UX](task-55-quota-usage.md) | workload/service quota、利用量と操作UX | 40, 47, 50 |

## 直近の実装状態（2026-10-10確認）

- Workload RunのDB/domain、Tenant visibility、create/list/detail/cancel APIとCLI、transactional Outbox、専用scheduler SQSへのDispatcher、厳格decode・visibility延長・成功後ackするconsumer coreは実装済み。
- Go testを隔離Podman containerで実行するFake Runtime、commit照合、redacted logs、S3 writer、state/artifact Serviceは実装済み。ただしServiceはSQS workerに未接続で、checkout/worker deploymentもない。
- `enable_workload_run_creation`はdefault-off。worker/handlerの運用可能性を確認せず有効化しない。
- Task 37の資格情報libraryは実装・検証済み。App private keyはAWSCURRENTから取得し、Tenant/Repository認可とlive Installation検証後にread-only tokenを発行する。専用service/IAM配備とcheckout接続は未実装。
- Resource PlaneのWorkspace Lambdaは明示opt-inのFake Runtime用。実AWS MicroVM Runtime、durable fencing、EFS、Gatewayは未実装。
- Placement policy/capacity/healthのDB・Schedulerロジックはあるが、Resource Planeからの自動health報告と本番AWS検証は未実装。
- Tenant別App/三種類のSecret/承認reader Roleの設計を追加（Tasks 56–60）。Task 56でmetadata schemaとAPI-roleのDB grantを追加したが、現行GitHub App設定やAWS IAMのTenant分離は未変更。
- Task 56のmetadata/API/CLI基盤とPostgreSQL integration validationが完了。Owner/Admin制御、inert draft、idempotency、revision付きdisable、監査、default-off flag、operator-only binding registration手順を追加。Tenant App機能は未有効化。Tasks 57–60は未完了。
- Task 38の固定commit checkout providerを実装・検証済み。Tenant/Installation/Repository認可後にのみ資格情報を要求し、askpass FD経由でtokenを渡す。専用packageのstatement coverageは83.6%。Task 39はCP側のdurable dispatch handoffから着手中。
- README Phase 0–9には残項目が他にもある。上記Task 36–60がその実行順と受入条件を具体化する。完了判定時はREADMEの該当チェックとこのboardを両方更新する。

## 実装の完了定義

コードがあるだけでは完了にしません。各taskファイルに定めた受入条件を満たし、ユニット/integration/E2Eのうち適切なテストが通り、セキュリティ境界と未対応制約がドキュメント化されている必要があります。AWS applyや実環境cutoverなど外部変更を要する項目は、実装済みでも運用検証と区別して未完了のまま記録します。
