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
| 1 | [36 — Worker実行基盤ADR](task-36-workload-worker-adr.md) | Superseded：旧EC2決定は撤回。EC2は対象外 | なし |
| 2 | [37 — GitHub App資格情報](task-37-github-app-credentials.md) | 完了（library）：AWSCURRENT、JWT、Repository限定read-only token。配備は40 | なし |
| 3 | [38 — 固定commit checkout](task-38-workload-source-checkout.md) | 完了：Tenant/repository認可後に固定SHAを安全にcheckout、coverage 83.6% | 37（完了） |
| 4 | [39 — Workload実行handler](task-39-workload-execution-handler.md) | CP/RP library実装済み。実agent・worker/recovery配備と接続は未完了 | 38 |
| 5 | [40 — Worker/IAM/Artifact配備](task-40-workload-worker-deployment.md) | Lambda MicroVM worker、Queue、S3/IAM/監視を配備 | 61, 63 |
| 6 | [41 — GitHub Checks](task-41-github-checks.md) | queued/in-progress/completed結果とcommit照合 | 37, 39 |
| 7 | [51 — Webhookイベント運用](task-51-webhook-events.md) | PR等のRepository Event正規化、Delivery監査、retention/replay | 37, 39 |
| 8 | [42 — Factory end-to-end](task-42-software-factory-e2e.md) | Installからisolated test、Check、log、cleanupを検証 | 38–41, 51 |

### P0追加 — Tenant別GitHub App（今回の要望で優先）

設計は[ADR 0007](../docs/adr/0007-tenant-github-apps.md)と[Tenant App仕様](../docs/tenant-github-apps.md)。Tenant-ownedのactivationは後続の資格情報検証・Webhook・移行が終わるまでdefault-off。Task 56のmetadata/API基盤は完了し、次はTask 57。

| Task | 内容 | Depends on |
|---|---|---|
| [56 — Tenant App Registry/API](task-56-tenant-github-app-registry.md) | 完了：metadata、Owner/Admin API/CLI、承認binding、inert draft | 37（完了） |
| [57 — Tenant資格情報resolver](task-57-tenant-github-credential-resolver.md) | 進行中：App proof API、active/draft resolver、exact reader provider、revision/generation cache partition/per-use authorizationとRun identity pin contractを実装。production checkout broker wiringが残る | 56 |
| [58 — Tenant OAuth/Webhook](task-58-tenant-github-setup-webhooks.md) | 進行中：opaque ingress/HMAC/route単位dedup実装。OAuth state/provider wiringとDB generation recheckが残る | 56, 57 |
| [59 — 移行/IAM](task-59-tenant-github-migration-iam.md) | 進行中：Installation binding/Run immutable credential pin実装。backfill/drain/exact IAM/rollback検証が残る | 56–58 |
| [60 — Tenant App E2E/運用](task-60-tenant-github-app-e2e.md) | 二Tenant越境拒否、rotation、migration、shared回帰 | 56–59 |

### P0追加 — Workload実行基盤の再選定（EC2対象外）

実行基盤はユーザー指定のLambda MicroVM。旧EC2決定と先のFargate選定は撤回済み。公式専用APIとSDKを確認し、Tasks 63–65のcontroller/cancel/result recovery libraryを実装した。AWSは8時間上限で標準egressはInternet。adapterは8時間超を拒否し、承認済みVPC connectorを必須にする。image/container/network PoCは未実施。Task 40の配備はその確認に依存し、Run作成はdefault-off。設計は[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。

| Task | 内容 | Depends on |
|---|---|---|
| [61 — Lambda MicroVM実行契約の確認](task-61-workload-runtime-selection.md) | API確認済み。8h/ARM64/network制約を記録。実image/container/network PoC待ち | 39（substrate-neutral foundation）、43と連携 |
| [62 — RP commandのdurable受理](task-62-workload-command-acceptance.md) | 完了（library）：strict decode、RP固定、条件付きjournal作成、duplicate/identity検証 | 39 journal（実装済み） |
| [63 — Lambda MicroVM controller/recovery](task-63-lambda-microvm-controller.md) | 実装（library）：公式SDK、仕様/ID binding、Ensure/Poll、結果のdurable再送。agent/worker配備待ち | 61, 62 |
| [64 — cancel/物理cleanup](task-64-workload-cancellation-cleanup.md) | 実装（core）：CP cancel Outbox、RP tombstone、停止確認、retry、replacement gate | 63 |
| [65 — 障害復旧/local E2E](task-65-workload-recovery-tests.md) | 実装：fake/SDK stub、race、PostgreSQL RP→CP結果とcancel統合 | 63, 64 |

### 再整理後の着手順

1. Tasks 62–65のlibrary/local検証は実装済み。単独のjournal受理だけでqueueをackしない。
2. 次はTask 45の認証付きAgentRunner・trusted source/result搬送と、Task 61/43のimage/container/network PoC。同時に着手可能なAPI/CLI側タスクは57。
3. Task 40でtrusted resolver、SQS command/result publisher、durable recovery schedulerを配備し、39の実worker統合を完了する。
4. Task 41のChecksと51のイベント処理を接続し、実環境Factory E2E42へ進む。

実装済みlibrary、未接続のruntime、配備/実環境検証を別々に追跡する。TerraformよりAPI/CLIとcontrollerの実装・テストを先行する。

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
- Go testを隔離Podman containerで実行するFake Runtime、commit照合、redacted logs、S3 writer、state/artifact ServiceとTask 38固定commit checkoutを実装済み。Task 39ではCP result handler/result SQS consumerと、条件付きS3 CASを使うRP journal primitiveも追加したが、Fake Runtime Service/SQS workerおよび実Resource Plane executor/controllerとは未接続。
- `enable_workload_run_creation`はdefault-off。worker/handlerの運用可能性を確認せず有効化しない。
- Task 37の資格情報libraryは実装・検証済み。App private keyはAWSCURRENTから取得し、Tenant/Repository認可とlive Installation検証後にread-only tokenを発行する。専用service/IAM配備とcheckout接続は未実装。
- Resource PlaneのWorkspace Lambdaは明示opt-inのFake Runtime用。実AWS MicroVM Runtime、durable fencing、EFS、Gatewayは未実装。
- Placement policy/capacity/healthのDB・Schedulerロジックはあるが、Resource Planeからの自動health報告と本番AWS検証は未実装。
- Tenant別App/三種類のSecret/承認reader Roleの設計を追加（Tasks 56–60）。Task 56でmetadata schemaとAPI-roleのDB grant、Task 57でactive/draft resolver、STS/Secrets Managerのexact Role/Secret/Version provider、integration/revision/generation単位のcache identityとper-use active generation authorization、外部proofとDB OCC再検証を分けたverify APIを追加した。Run integration接続、現行GitHub App設定やAWS IAMのTenant分離は未変更。
- Task 56のmetadata/API/CLI基盤とPostgreSQL integration validationが完了。Owner/Admin制御、inert draft、idempotency、revision付きdisable、監査、default-off flag、operator-only binding registration手順を追加。Tenant App機能は未有効化。Tasks 57–60は未完了。
- Task 57でTenant credential proof/resolver/cache safetyを追加。Task 58ではopaque per-integration Webhook handlerとintegration-scoped inbox keyを追加したが、AWS-backed Webhook secret resolver、OAuth setup pinning、queue処理時のgeneration再検証は残る。Tenant App activationは引き続きdefault-off。
- Task 59のschemaでTenant App/Installation bindingとWorkload Runのintegration/revision/generationを永続化。新規Runはserver-side bindingでpinされ、active Tenant AppがあるのにInstallation未bindingならshared credentialへfallbackしない。旧Runは明示的にshared/null identityのまま。AWS checkout brokerとpurpose-specific IAM、既存データbackfill/drainは未完了。
- Task 38の固定commit checkout providerを実装・検証済み。Tenant/Installation/Repository認可後にのみ資格情報を要求し、askpass FD経由でtokenを渡す。専用packageのstatement coverageは83.6%。Task 39はCP側のdurable dispatch handoff/result処理、Tasks 62–65はRP journal・公式SDK controller・cancel/cleanup・復旧テストまで実装。基盤はLambda MicroVM、EC2は対象外。Task 61のimage/network PoC、Task 45の実agent連携、Task 40のworker/recovery配備まで実行機能はdefault-off。
- Task 62のRP command受理libraryを追加。条件付きjournal作成、重複時のidentity照合、cancel/cleanup checkpoint保持を実装。Tasks 63–65でcontroller/公式SDK adapter、CP cancel Outbox、物理cleanup/retry、durable result replayとlocal/PostgreSQL統合テストを実装。実agent/queue/recovery scheduler配備とAWS PoCは未完了。
- README Phase 0–9には残項目が他にもある。上記Task 36–65がその実行順と受入条件を具体化する。完了判定時はREADMEの該当チェックとこのboardを両方更新する。

## 実装の完了定義

コードがあるだけでは完了にしません。各taskファイルに定めた受入条件を満たし、ユニット/integration/E2Eのうち適切なテストが通り、セキュリティ境界と未対応制約がドキュメント化されている必要があります。AWS applyや実環境cutoverなど外部変更を要する項目は、実装済みでも運用検証と区別して未完了のまま記録します。
