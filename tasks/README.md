# Hako Implementation Task Board

このディレクトリは、次の実装担当（Lunnaを含む）が一つずつ着手できる粒度にしたタスクの実行台帳です。設計上のSystem of Recordは引き続き[README](../README.md)、コンポーネント仕様は各`docs/`です。ここでは未完了項目を依存関係・境界・受入条件に整理します。

## 着手ルール

1. `P0`から順に、依存タスクが完了しているものを一件だけ選ぶ。
2. 着手前に`git status`、対象コード、タスク記載の仕様を確認する。既存変更と常時起動のPostgresを保持する。
3. タスクの受入条件を実装・テストし、重要な仕様変更は関連`docs/`とREADMEの状態記述へ反映する。
4. AWS `apply`、実AWSリソース削除、秘密情報の取得/表示は別途明示承認がない限り行わない。Podmanを使うローカルテストは許可されているが、DBコンテナを停止しない。
5. 終了時に検証結果・未完了項目・commit hashを報告する。別タスクへ範囲を広げず、次の依存可能タスクを示す。

## 作業中タスクの整理（2026-10-11）

Task 41のGitHub Checks delivery（mTLS broker/consumer、idempotent Check更新、Queue/DLQ/IAM/alarmとdefault-off service Terraform）はローカル実装・検証済みで、production Secret/certificate/network/image/deployment gate待ち。Task 51/78/79のWebhook基盤・operator API/runbook・包括lifecycle PostgreSQL E2Eもローカル検証済み。Tenant GitHub App Tasks 66–72とTask 68はローカル実装・検証済みだが、production certificate/IAM/worker wiring/cutoverは未実施。AWS apply、実Secret取得、実GitHub設定変更はしていない。

親タスク57–60/50の残作業をTask 66–71へ分割した。OAuth broker通信をTask 72、Claude Code ActionをTask 73–77、Webhook Task 51の残件をTask 78–79へ分割する。親は集約用に残すため、残件数を数えるとき親と子を二重計上しない。子タスクはリポジトリ全体の残件数ではない。

| 順序 | 実行タスク | 親 | 依存・状態 |
|---|---|---|---|
| 1 | [66 — OAuth trusted broker](task-66-tenant-oauth-broker.md) | 58 | Implemented locally：DB resolver/Secret reader、OAuth/checkout broker compositionあり。production IAM/certificate/deploymentが残る |
| 2 | [72 — OAuth broker mTLS通信](task-72-oauth-broker-mtls.md) | 66 | Implemented locally：TLS 1.3/mTLS、OAuth/checkout adaptersとhandshake testsあり。証明書発行/rotation・production networkが残る |
| 3 | [67 — OAuth API/CLI接続](task-67-tenant-oauth-cli-wiring.md) | 58, 66, 72 | Implemented locally：startup mTLS設定/CLI setup、mTLS broker→API→PostgreSQL callback E2E (fake upstream)通過。production certificate/network wiringが残る |
| 並行可能 | [68 — Webhook abuse制御](task-68-webhook-abuse-controls.md) | 50, 58 | Implemented locally：preflight/DSQL limit/Gateway throttleとunit・PostgreSQL検証済み。alerting/deployment reviewは未完了 |
| 4 | [69 — proof/checkout broker接続](task-69-tenant-checkout-broker.md) | 38, 57, 59 | Implemented locally：mTLS broker API、DB-backed immutable Run pin、exact credential reader、shared no-fallback routingとPostgreSQL検証。production process/worker wiringは40/66/72 |
| 5 | [70 — binding移行/cutover tooling](task-70-tenant-migration-cutover.md) | 59, 67, 69 | Implemented locally：revision/generation fence付きdry-run/backfill、run drain guard/default選択、rollback/idempotency検証。production cutoverは承認後 |
| 6 | [71 — 二Tenant local E2E](task-71-tenant-app-local-e2e.md) | 60, 67–70 | Implemented locally：並行callback、Tenant別Webhook/Run pin/checkout、replay・cross-App/Tenant拒否、rotation/disable/shared回帰とOCC retry。production rolloutは未検証 |
| 後続 | [73 — Claude Code Action親タスク](task-73-claude-code-action.md) | 52 | Todo：Task 74–77に分割。Runner/worker/Checks/security foundation後 |
| 完了（ローカル） | [78–79 — Webhook運用/E2E](task-78-webhook-platform-operator-dlq.md) | 51 | Operator API/runbookとretention/replay/trigger lifecycle E2EをPostgreSQLで検証。Production運用ゲートは別 |

Tenant App子タスクのlocal受入条件はTask 70/71まで実装・検証済み。production broker/certificate/IAM/worker wiringと承認済みcutoverは未完了で、次の実装順は下の「整理後の実行キュー」に集約する。Task 68のローカル実装も完了扱いだが、監視・AWS配備レビューは別の運用作業として残る。Terraform配備はAPI/CLIとローカル検証の後段とする。Workspace/EFS/Gateway（43–47）、運用（48–50）、Software Factory拡張（53–55）も未完了。親子タスクは二重計上せず、詳細は各taskの受入条件を参照する。

### ローカル実装と外部承認の境界

- ローカルで進める：broker adapter、API/CLI接続、fake/Postgres E2E、移行dry-run、runbook。
- 外部変更前に承認が必要：実Secret取得、Role/Secret provisioning、AWS apply、GitHub設定変更、Tenant activation/cutover。
- 未検証：実MicroVM/container/network/EFS隔離、Gateway replay/revocation、実AWS recovery。libraryテスト合格を配備完了として扱わない。

## 優先順位と依存関係

### P0 — Software Factoryの安全な最小実行経路

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 1 | [36 — Worker実行基盤ADR](task-36-workload-worker-adr.md) | Superseded：旧EC2決定は撤回。EC2は対象外 | なし |
| 2 | [37 — GitHub App資格情報](task-37-github-app-credentials.md) | 完了（library）：AWSCURRENT、JWT、Repository限定read-only token。配備は40 | なし |
| 3 | [38 — 固定commit checkout](task-38-workload-source-checkout.md) | 完了：Tenant/repository認可後に固定SHAを安全にcheckout、coverage 83.6% | 37（完了） |
| 4 | [39 — Workload実行handler](task-39-workload-execution-handler.md) | CP/RP library実装済み。実agent・worker/recovery配備と接続は未完了 | 38 |
| 5 | [40 — Worker/IAM/Artifact配備](task-40-workload-worker-deployment.md) | Lambda MicroVM worker、Queue、S3/IAM/監視を配備 | 61, 63 |
| 6 | [41 — GitHub Checks](task-41-github-checks.md) | **現在作業中**：ローカル配信E2E、default-off queue/DLQ、Dispatcher/consumer IAM、Fargate task/service、mTLS Secret reference、DSQL grants、任意SNS alarm通知を追加。AWS deployment/network/Secret provisioning reviewが残る | 37, 39 |
| 7 | [51 — Webhookイベント運用](task-51-webhook-events.md) | ローカル基盤実装・PostgreSQL検証済み：Repository Event正規化、7日raw payload retention、Owner/Admin監査付きreplay API・redacted failed-delivery API/CLI、concurrent processing fence、stable Delivery ID→Run/Check/Outbox作成、Repository別default-off triggerとpre-opt-in delivery抑止。operator dead-letterと包括retention/replay E2Eが残る | 37, 39 |
| 8 | [42 — Factory end-to-end](task-42-software-factory-e2e.md) | Installからisolated test、Check、log、cleanupを検証 | 38–41, 51 |

### P0追加 — Tenant別GitHub App（今回の要望で優先）

設計は[ADR 0007](../docs/adr/0007-tenant-github-apps.md)と[Tenant App仕様](../docs/tenant-github-apps.md)。Task 56と子タスク70/71までのlocal実装・受入検証は完了。残りはbroker/certificate/IAM/worker wiringと、承認後のproduction migration/cutoverであり、Tenant-owned activationはdefault-offを維持する。

| Task | 内容 | Depends on |
|---|---|---|
| [56 — Tenant App Registry/API](task-56-tenant-github-app-registry.md) | 完了：metadata、Owner/Admin API/CLI、承認binding、inert draft | 37（完了） |
| [57 — Tenant資格情報resolver](task-57-tenant-github-credential-resolver.md) | local resolver/Run pin/brokerは実装済み。production broker process/worker/IAM wiringが残る | 56 |
| [58 — Tenant OAuth/Webhook](task-58-tenant-github-setup-webhooks.md) | local webhook resolver/OAuth callback/API/CLI接続は実装済み。production certificate lifecycle/Role provisioningが残る | 56, 57 |
| [59 — 移行/IAM](task-59-tenant-github-migration-iam.md) | binding/immutable Run pinとTask 70 migration toolingはlocal検証済み。production IAM/backfill/cutover/rollbackは承認後 | 56–58 |
| [60 — Tenant App E2E/運用](task-60-tenant-github-app-e2e.md) | Task 71の二Tenant local E2Eは検証済み。production rollout確認と運用runbookが残る | 56–59 |

### P0追加 — Workload実行基盤の再選定（EC2対象外）

実行基盤はユーザー指定のLambda MicroVM。旧EC2決定と先のFargate選定は撤回済み。公式専用APIとSDKを確認し、Tasks 63–65のcontroller/cancel/result recovery libraryを実装した。AWSは8時間上限で標準egressはInternet。adapterは8時間超を拒否し、承認済みVPC connectorを必須にする。image/container/network PoCは未実施。Task 40の配備はその確認に依存し、Run作成はdefault-off。設計は[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。

| Task | 内容 | Depends on |
|---|---|---|
| [61 — Lambda MicroVM実行契約の確認](task-61-workload-runtime-selection.md) | API確認済み。8h/ARM64/network制約を記録。実image/container/network PoC待ち | 39（substrate-neutral foundation）、43と連携 |
| [62 — RP commandのdurable受理](task-62-workload-command-acceptance.md) | 完了（library）：strict decode、RP固定、条件付きjournal作成、duplicate/identity検証 | 39 journal（実装済み） |
| [63 — Lambda MicroVM controller/recovery](task-63-lambda-microvm-controller.md) | 実装（library）：公式SDK、仕様/ID binding、Ensure/Poll、結果のdurable再送。agent/worker配備待ち | 61, 62 |
| [64 — cancel/物理cleanup](task-64-workload-cancellation-cleanup.md) | 実装（core）：CP cancel Outbox、RP tombstone、停止確認、retry、replacement gate | 63 |
| [65 — 障害復旧/local E2E](task-65-workload-recovery-tests.md) | 実装：fake/SDK stub、race、PostgreSQL RP→CP結果とcancel統合 | 63, 64 |

### 整理後の実行キュー

親タスクと分割済み子タスクを重複計上しない。ここでは次に着手する実装可能項目と、Claude Code Actionまでの依存順を示す。外部AWS/GitHubの変更・本番有効化は別途承認が必要。

| 順 | タスク | 状態と次の作業 | 完了条件 |
|---|---|---|---|
| 完了（ローカル） | [79 — Webhook lifecycle E2E](task-79-webhook-lifecycle-e2e.md) | signed ingressからRun/Check/Outbox terminal acknowledgmentまでPostgreSQL/fake E2E、focused Make target、runbookを追加 | `make test-webhook-lifecycle-integration`と関連integration suiteで検証 |
| — | [78 — Platform operator DLQ](task-78-webhook-platform-operator-dlq.md) | **ローカル実装・focused PostgreSQL検証済み**。production operator-group provisioning/deploymentは別ゲート | cross-Tenant redacted listing、`hako-admin` authorization、expiry/active-binding replay、atomic audit、rate limit、runbook |
| 3 | [41 — GitHub Checks](task-41-github-checks.md) | **ローカル実装済み・運用ゲート待ち**。専用Queue/DLQ、consumer task/service、roles、Secret ARN/mTLS注入、DSQL権限、任意SNS通知をdefault-off Terraform化。AWS apply/Secret作成は別承認 | network/Secret/image inputsのreview・approved deploymentと運用確認（apply前は未完了） |
| 4 | [80 — Runtime契約](task-80-workspace-runtime-contract.md) → [81 — durable journal/fence](task-81-workspace-runtime-journal.md) → [82 — Lambda MicroVM adapter](task-82-workspace-lambda-microvm-adapter.md) → [83 — recovery/fake API E2E](task-83-workspace-runtime-recovery-e2e.md) | **Tasks 80–83 local実装・検証済み**：SDK HTTP fake、adapter再起動/timeout、foreign ID拒否、cleanup retry、concurrent duplicate、Controller→result-consumer→PostgreSQL binding投影。実AWS PoC・本番接続は別ゲート。EC2は対象外 | Workspace runtime suiteと`TestWorkspaceControllerResultConsumerProjectsBindingEndToEnd`。AWS bucket/IAM/image/network/lifecycle PoCは承認後 |
| 4a | [45 — hako-agent](task-45-hako-agent.md), [61 — MicroVM PoC](task-61-workload-runtime-selection.md) | **未完了**。固定`/bin/sh`のLinux PTY manager、Gateway ticket検証/WSSからPTYまでのlocal E2E、verified Workspace claimで振り分けるprocess-local routerを追加。journal-backed per-stream runtime lookup/dialer、Lambda image builderの実ビルド、heartbeat/exec、production key distribution、network/MicroVM PoCは残る | local artifact/hook/PTTY/ticket/PostgreSQL API testsと、承認後の実MicroVM PoCを区別 |
| 5 | [40 — Worker deployment](task-40-workload-worker-deployment.md) | **未完了**。credential/protocol/process、Agent listener binary、Tenant-pinned mTLS broker checkout（shared fallback拒否）、Agent→Controller ticket付きredacted log relay、Controller側attempt限定S3 writer、handler composition、専用command/result queue・DLQ・alarms・queue-only IAM・Dispatcher fail-closed routingとfake E2Eは実装済み。Terraformにdefault-offのcombined Worker Lambda、command mapping、recovery schedule、alarmを追加。今回、opt-in deploymentへprivate subnet/専用SGの必須入力、Lambda VPC attachment、指定subnet/SGに制限したENI IAMを追加しmock planで検証。VPC endpoints/NAT/route/SG reachability、bucket/KMS policy、broker/image/network gate、AWS deploymentは未検証 | local integration、Terraform validate/mock plan、runbook。実AWS適用は承認後 |
| 6 | [42 — Software Factory E2E](task-42-software-factory-e2e.md) | **未完了**。signed webhook→ChecksのPostgreSQL/Podman E2E、Scheduler Consumer→placement/Outbox→RP Dispatcher→Lambda Worker→署名付きAgent success、result apply/replay、handler restart replayを検証。cancel Outbox→Dispatcher→Worker terminateで署名付きguest Agent process contextがcancelされ、PostgreSQLへ結果反映・artifactなしを検証。S3Store+conditional fake S3によるscheduled recoveryとACK消失後のPostgreSQL再適用も検証。AWS smoke/実環境検証は残る | `make test-software-factory-e2e`と`go test -tags=integration ./internal/workloadcontroller -run Composed` |
| 7 | [52 — Ephemeral Actions runner](task-52-ephemeral-actions-runner.md) | **進行中**。Repository別default-off admission、Lambda MicroVM launcher、Plane mTLS launch/bind/token/failure、署名bootstrap、Tenant model credential broker、guest `/run` identity-bound receiverを実装・unit/race test。`cmd/hako-actions-runner-guest`はofficial runnerを`--ephemeral`設定し、model keyはrunner process envのみ、完了後にguest local stateを削除。configure/start/run失敗metadataはRunner診断用に残る。GitHub観測workflow_jobのstatusからdurable Checkを更新し、startup failure Checkをshared outbox/consumerから配信する経路をTask 76で追加、PostgreSQL/fake-GitHubまで検証済み。guestからfake Runnerまでのcomposed local E2Eあり。ARM64 image recipeあり。Pinned archive/hash/keyなしでimageは未build、AWS apply/live verificationも未実施。[queue and Check contract](../docs/actions-runner-startup-queue.md) | duplicate/misassignment/fork/timeout/cancel/startup failure/cleanup retry/MicroVM fence |
| 8 | [74 — Provenance](task-74-claude-action-provenance.md) → [75 — Tenant credential policy](task-75-claude-tenant-credential-policy.md) | **Task 74 local launch gate implemented。Task 75進行中**：Tenant API/CLI、exact-Version reader、provenance/Plane/MicroVM fence、mTLS broker→Worker→signed bootstrap、guest process env handoff/local-state cleanupをfake/unit test済み。Pinned image build、full E2E、IAM/certificate配備は残る | 不正workflow/fork/cross-Tenant/foreign VM拒否。Secretはfake/localテストだけ |
| 9 | [76 — Action execution](task-76-claude-action-execution.md) → [77 — E2E/operations](task-77-claude-action-e2e.md) | **Task 76進行中**。provider `/run`→署名bootstrap→one-shot official Runner process consumerと、workflow job/attemptに結びつくdurable GitHub Check、startup-failure Check、shared consumer deliveryを実装・PostgreSQL/fake GitHubで検証済み。durable terminal cleanup、pinned image build/publish、authorized webhookからcleanupまでのfull E2Eは未完了 | Fake provider/runner/GitHub + PostgreSQL E2E、失敗時cleanup、運用・security文書 |

Task 41とTask 51のローカル受入は完了しproduction deployment gate待ち。Task 78/79のWebhookローカル作業は完了。Tenant App Tasks 66–72はローカルのAPI/CLI/broker/mTLS/E2E基盤が実装済みで、本番証明書/IAM/worker配備は別の運用ゲートとして追跡する。Task 68もlocal controlsは実装済みだが、alerting/deployment reviewは残る。

Claude Code Actionの完全な実行機能は**まだ未完了**。Task 74のprovenance gateとTask 75のTenant別resolver・mTLS broker・署名付きguest bootstrap handoff/official runnerへのprocess環境引き渡しはlocal実装済みだが、pinned image、durable terminal cleanup、authorized webhookからCheck/cleanupまでのfull E2E、production IAM/certificateは残る。Tasks 76–77（実行・E2E/operations）も未完了。Task 52のephemeral runnerとTask 40のLambda MicroVM workerが前提で、既存GitHub App credentialやAWS credentialをClaudeへ流用しない。[Workflow contract](../docs/claude-code-action.md)と[credential boundary](../docs/claude-model-credentials.md)を参照。

実装済みlibrary、未接続runtime、local E2E、production deploymentを別々に追跡する。API/CLI/controllerとlocal testsを優先し、Terraformはdeployment contractの検証に限定して先行applyしない。

### P1 — Workspaceの実AWS Runtimeと永続データ

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 9 | [43 — Workspace Runtime](task-43-workspace-runtime.md) / Tasks [80–83](task-80-workspace-runtime-contract.md) | ADR/contract → durable fencing → MicroVM lifecycle → recovery/E2E | Task 61 runtime evidence |
| 10 | [44 — EFS Workspace保存](task-44-efs-workspace.md) | **進行中**：POSIX-restricted AP manager、immutable journal binding、Lambda MicroVM provision/reuse/cleanup、strict `/run` hook、非特権Agent childを実装。暗号化EFS、exact role/AP IAM/TLS policy、private mount targets、connector-SG限定NFS、日次Backup/35日retentionをdev rootへdefault-offで接続。動的Workspace role/AP lifecycle、private subnet/connector実証、restore/orphan/live MicroVM検証は残る | 43 |
| 11 | [45 — hako-agent](task-45-hako-agent.md) | 固定`/bin/sh` PTY、VS Code/SSH fixed-loopback bridge（8081/22）、Guest WSS/one-use MicroVM-bound ticket、EFS後の非特権Agent child、mode-0600 Unix socket/readiness/route proxy、pinned/checksummed code-server ARM64プロセス、journal→GetMicrovm per-stream resolverを実装・local test。production Gateway/live MicroVM、実`sshd`/SFTP/host-key配布、actual image/`/dev/pts`、heartbeat/exec、production key provisioningは未接続 | 43 |
| 12 | [46 — Session/Gateway](task-46-session-gateway.md) | 進行中：5分ticket、RBAC API issue/revoke、HTTPS/HMAC active-session API/client、TLS-only WSS core/PTY/port-forward E2E、journal-backed Lambda MicroVM shell resolver、15秒周期のactive-session再検証によるstream中のrevoke/expiry反映を実装。`cmd/hako-workspace-gateway`でAWS task-roleベースのGateway composition、mounted key/config validation、non-root ARM64 imageを追加。Terraform deployment/least-privilege IAM、鍵rotation、fleet-wide limits/metrics/live AWS E2Eが残る | 43, 45 |
| 13 | [47 — 開発者CLI/Service Catalog](task-47-developer-ux.md) | **進行中**：`hako open`のloopback browser tunnel、VS CodeとSSHの固定Guest bridge、専用Session/Agent ticket、Workspace imageでpinned/checksummed code-serverをEFS UID/GIDで起動する経路を追加。ARM64 image build/MicroVM検証とGuest `sshd`/SFTP/host-key provisionは未実施。`hako shell`のLinux terminal resize/framing、`hako wait`/`forward`/`ssh-proxy`/`cp`、versioned PostgreSQL/Redis catalogと認可API/CLIも実装。Catalog操作はdesired metadata onlyでruntime executor未接続。health/reconciliation、production routerが残る | 44–46 |

### P2 — Placementの自動運用、AWS検証、運用準備

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 14 | [48 — Health reporterとplacement運用](task-48-health-reporter.md) | **ローカル実装・unit/PostgreSQL検証完了**。paired Secret provisioning/rotation、AWS deployment、live probe/alarms verificationは未実施・承認待ち | なし |
| 15 | [49 — Terraform/AWS E2E](task-49-aws-environment-e2e.md) | Account分離環境、実AWS apply手順、作成〜接続〜cleanup検証 | 43–46, 48 |
| 16 | [50 — Security/Operations](task-50-security-operations.md) | 進行中：既知Go脆弱性11件修正、OAuth redirect拒否、CI govulncheckとlocal review。監査/復旧/実AWS検証が残る | 40, 42, 49 |

### P3 — Software Factoryの拡張と利用制御

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 17 | [52 — GitHub Actions Runner Mode](task-52-ephemeral-actions-runner.md) | 進行中：Plane-specific mTLS broker deregistration、live cleanup lease revalidation、cleanup worker、Lambda MicroVM SDK terminatorを実装。runner launch、consumer deployment/live AWS検証が残る | 40, 41, 51 |
| 18 | [53 — Native Pipeline/Agent](task-53-native-pipeline-agent.md) | **進行中**。version 1 manifest、triggerのRun→Lambda hook→署名ticket伝播、固定commit上のpipeline選択・固定catalog argv実行、invalid manifestのterminal failureまでローカル接続。選択pipelineのRun永続化/per-pipeline Checks、runtime resource enforcement/services、Issue→Agent→PRは未完了 | 39, 50, 51 |
| 19 | [54 — Preview/Catalog/Deploy](task-54-preview-catalog-deploy.md) | Preview、Software Catalog、OIDC deploy policy | 43–46, 50, 51 |
| 20 | [55 — Quota/Usage UX](task-55-quota-usage.md) | workload/service quota、利用量と操作UX | 40, 47, 50 |
| 後続 | [73–77 — Claude Code Action](task-73-claude-code-action.md) | Task 52のephemeral runner上でworkflow provenance、Tenant model credential policy、実行/cleanup、local E2Eを実装 | 40, 41, 50–52 |

## 直近の実装状態（2026-10-17確認）

- Workload RunのDB/domain、Tenant visibility、create/list/detail/cancel APIとCLI、transactional Outbox、専用scheduler SQSへのDispatcher、厳格decode・visibility延長・成功後ackするconsumer coreは実装済み。
- Task 42のローカルPostgreSQL + Podman E2Eで、Tenant-specific production HTTP handlerとDB Installation authorizer（Secretはsynthetic resolver）を通る署名付きWebhookからHMAC拒否、foreign Installation拒否、Delivery ID重複/conflict、Tenant外Repository IDのRun生成拒否、成功/失敗immutable untrusted Run、pre-dispatch cancelでplacement/attemptを作らない経路、executor停止後のin-flight cancellation、timeoutとartifact upload failure、redacted artifact、success/failure/cancelled/timed_out Checks、GitHub 503後のCheck再試行をBatchSize=1で接続・検証した。成功/失敗Podman containerはRunラベルで`--rm`後に不在確認し、新しいExecutorによるterminal Run再配信もPostgreSQL stateで拒否する。Factory E2E内でforeign TenantからのRun取得拒否と同一Tenant内の無関係requesterへのlist非表示も確認し、別のAPI integrationは認証済みroute境界を検証する。加えてPostgreSQL Scheduler Consumer→placement/Outbox→RP Dispatcherの専用Queue route→Lambda Worker handler→MicroVM fake→署名付きAgent→PostgreSQL `ResultHandler` の結果適用/重複再送/ログartifact/cleanup E2Eを検証した。S3Store上のrecoverable journalをWorker handler再生成後のscheduled recoveryで読み、ACK消失後の結果を再適用する経路もconditional fake S3とPostgreSQLで検証済み。cancel request→cancel Outbox→Dispatcher→Lambda Worker→MicroVM terminate時に署名付きAgentの実プロセスが`context.Canceled`を受ける経路もfake provider/guestで確認し、cancelled resultとartifactなしをPostgreSQLで検証した。実AWS検証は未完了。CIはPodmanとGo imageを必須化。
- Go testを隔離Podman containerで実行するFake Runtime、commit照合、redacted logs、S3 writer、state/artifact ServiceとTask 38固定commit checkoutを実装済み。Task 39ではCP result handler/result SQS consumer、条件付きS3 CASを使うRP journal primitive、trusted `HakoEventType`属性とpartial batch failureを扱うWorkload Lambda SQS adapter、prefix-scoped S3 recovery listing、bounded multi-page recovery sweepを追加した。S3 conditional recovery lease、lease loss時に停止するsweep orchestration、設定上限必須のEventBridge Lambda adapterもローカル実装・検証した。Production resolver/agent/runtime composition、recovery Lambda composition・schedule、queue mappingとexact-object IAMは未接続。
- `enable_workload_run_creation`はdefault-off。worker/handlerの運用可能性を確認せず有効化しない。
- Task 37の資格情報libraryは実装・検証済み。App private keyはAWSCURRENTから取得し、Tenant/Repository認可とlive Installation検証後にread-only tokenを発行する。専用service/IAM配備とcheckout接続は未実装。
- Workspace用のLambda MicroVM adapter/library、durable fencing journal、provider IDのControl Plane binding projection、fake/API/DB E2E、Resource Controller Lambda内の明示`lambda-microvm` composition、Dispatcherのschema v2 operation routing、EFS AP provision/reuse/cleanup、strict `/run` hook付きWorkspace init/image定義はローカル実装済み。EFS helperを含むPodman image buildと実MicroVM hook、動的Workspace-scoped IAM role/AP lifecycle、EFS restore/live isolation、S3 journal/MicroVM IAMと実配備は未検証/未完了。EFS Terraformはdev rootでdefault-off、Workload Terraformは引き続きFake Runtime固定。
- Placement policy/capacity/healthに加え、Task 48のsigned HMAC ingress/client、replay protection、SQS queue・固定Lambda MicroVM image version・固定VPC connector・Controller function・EFS metadata probeを行うdefault-off scheduled Lambda、EMF metrics、exact-resource IAM/API Gateway throttle/alarmsを追加し、unit/PostgreSQL統合テストを検証済み。metadata probesはqueue処理、MicroVM実起動、network isolation、Workspace EFS mountを証明しない。ローカル実装は完了。paired Secret provisioning/rotation、実配備/alarms確認は外部運用ゲートとして未完了。
- Tenant別App/三種類のSecret/承認reader Roleの設計を追加（Tasks 56–60）。Task 56でmetadata schemaとAPI-roleのDB grant、Task 57でactive/draft resolver、STS/Secrets Managerのexact Role/Secret/Version provider、integration/revision/generation単位のcache identityとper-use active generation authorization、外部proofとDB OCC再検証を分けたverify APIを追加した。Run integration接続、現行GitHub App設定やAWS IAMのTenant分離は未変更。
- Task 56のmetadata/API/CLI基盤とPostgreSQL integration validationが完了。Owner/Admin制御、inert draft、idempotency、revision付きdisable、監査、default-off flag、operator-only binding registration手順を追加。Tenant App機能は未有効化。Tasks 57–60のlocal実装は子タスク66–72で分割・検証済みだが、production IAM/certificate/broker/worker配備と承認済みcutoverは未完了。
- Task 57でTenant credential proof/resolver/cache safety libraryを追加。API binaryはprivate-key Secret reader Roleを直接assumeせず、Task 69でtrusted mTLS checkout brokerを追加した。Task 58でWebhook resolver/authorizer/fence、default-off route、OAuth setup pin/callback adapterを追加し、Task 67でAPI/CLI mTLS adapterを接続した。production broker composition/certificate lifecycleと実Role/Secret provisioningは残り、Tenant App activationはdefault-off。
- Task 59のschemaでTenant App/Installation bindingとWorkload Runのintegration/revision/generationを永続化。新規Runはserver-side bindingでpinされ、active Tenant AppがあるのにInstallation未bindingならshared credentialへfallbackしない。Task 69 local checkout brokerはpinをDBから再解決し、shared/owned providerを分離する。旧Runは明示的にshared/null identityのまま。broker/worker deployment、purpose-specific IAM provisioning、既存データbackfill/drainは未完了。
- Task 38の固定commit checkout providerを実装・検証済み。Tenant/Installation/Repository認可後にのみ資格情報を要求し、askpass FD経由でtokenを渡す。専用packageのstatement coverageは83.6%。Task 39はCP側のdurable dispatch handoff/result処理、別HMAC keyで認証するtrusted execution-metadata APIとRP HTTP client、Resource Plane profileを使う`MetadataSpecificationResolver`、broker-aware checkoutをsource stagerへ渡す`RepositorySourceMaterializer`、決定的tar archiveとattempt-scoped暗号化S3 stager/extractor、operation ticket付きAgent転送・digest/commit/attempt検証まで実装した。Tenant-owned Run用のmaterializerは固定Tenant/Run pinをmTLS brokerへ渡し、shared credentialへのfallbackを拒否する。PostgreSQLでattempt/Placement/Tenant App pin/Run state再検証を確認した。Tasks 62–65はRP journal・公式SDK controller・cancel/cleanup・復旧テストまで実装。基盤はLambda MicroVM、EC2は対象外。Task 61のimage/network PoC、Task 45の実agent/image wiring、Task 40のworker/recovery配備まで実行機能はdefault-off。
- Task 62のRP command受理libraryを追加。条件付きjournal作成、重複時のidentity照合、cancel/cleanup checkpoint保持を実装。Tasks 63–65でcontroller/公式SDK adapter、CP cancel Outbox、物理cleanup/retry、durable result replayとlocal/PostgreSQL統合テストを実装。Recovery listing/replay primitivesも追加したが、実agent/queue/recovery scheduler配備とAWS PoCは未完了。
- Task 45/40向けに、二層Workload agent認証、protocol v1 client/handler、commit検証後に固定`go test ./...`のみを実行するprocess backend、port 8080 listener executable、public-key loader、ARM64 MicroVM ZIP packager、strict `/run` hookを追加。Agentはprovider VM IDとWorkload attempt/executionをpinし、別VM/attempt/execution ticketを拒否する。子process group cancellation、最小環境、bounded/redacted outputを実装した。Redacted logsはAgent内の有界メモリからoperation-scoped ticketでResource Plane Controllerが取得し、Controller側IAMでS3保存する。MicroVMへAWS/S3 credentialは付与しない。Workspace PTY managerは固定`/bin/sh`、Workspace cwd、最小環境、session/TTL/terminal/input bounds、resize、process-group cleanupを提供し、Gateway WSSから実PTYと許可済みloopback portへ流すlocal E2Eを追加した。HTTPS/HMAC active-session lookup API/clientもlocalでPostgreSQL検証済み。Production key/configuration、Gateway multi-runtime router、VS Code adapter、secret provider、Lambda image build/real run-hook PoC、worker deployment、egress isolationは未完了でWorkloadはdefault-off。
- README Phase 0–9には残項目が他にもある。Task 36–65と追加の実行単位66–79がその実行順と受入条件を具体化する。完了判定時はREADMEの該当チェックとこのboardを両方更新する。

## 実装の完了定義

コードがあるだけでは完了にしません。各taskファイルに定めた受入条件を満たし、ユニット/integration/E2Eのうち適切なテストが通り、セキュリティ境界と未対応制約がドキュメント化されている必要があります。AWS applyや実環境cutoverなど外部変更を要する項目は、実装済みでも運用検証と区別して未完了のまま記録します。
