# ADR 0006: Workload Worker実行基盤とdurable handoff

- Status: Accepted (design only; deployment not implemented)
- Date: 2026-10-04
- Scope: Native `job` / `untrusted` / `standard`の最初のGo test実行経路。Workspace Runtimeの選定はTask 43で別に扱う。

## Context

Workload APIのdeadlineは作成時から1秒〜24時間で、待機・checkout・起動も含む。現在のSchedulerConsumerはHandler終了までSQS receiptを保持するが、SQS visibilityは初回ReceiveMessageから最大12時間であり、heartbeatでもこの上限は延長できない。[AWS SQS processing guidance](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/best-practices-processing-messages-timely-manner.html)

既存のFake RuntimeはローカルPodmanで実行するだけで、本番のVM隔離、durable attempt、worker配備はない。Resource PlaneはControl PlaneのDSQLに直接接続せず、Control Planeにはcompute作成権限を与えない。

## Decision

最初のNative Jobは、Resource Planeの**使い捨てEC2 VM一台につき一つのRun attempt**で実行する。固定AMI上のtrusted supervisorがrootless Podmanを起動する。実行済みVM・ディスク・書き込みcacheは他Runに再利用しない。これはLambda MicroVM Runtimeの実装ではなく、Workload専用の代替実行基盤である。

| 所有者 | サービス・責務 |
|---|---|
| Control Plane | DSQLのRun/attempt/placement/Outbox、短時間scheduler handler、result consumer、deadline/recovery reconciler、GitHub App認可とChecks |
| Resource Plane | command SQS/DLQ、Resource Controller Lambda、EC2 lifecycle、private subnet/egress、使い捨てEBS、S3 execution journal/artifact、result送信、cleanup |
| Job VM | trusted supervisor、検証済みsource/cache、networkなしrootless Podman。DSQL、App private key、EC2作成・終了権限なし |

S3 execution journalはAWS副作用の重複・順序制御に限定する。Tenant/RBAC/RunのSystem of RecordやResource Plane独自DBにはしない。DSQLのみが論理状態の正本であり、journalとの不一致をreconcileする。

### 短時間dispatchと長時間executionを分離する

1. CP handlerはTenant/Run/repository/trust/deadlineをDBから再読込し、attempt ID、単調増加generation、固定Resource Plane、固定launch parameters、dispatch Outboxを同じtransactionで保存する。AWS操作はtransaction外。
2. scheduler receiptはこのdurable handoff後にackする。終端duplicateもack可能。単なるdecode成功ではackしない。checkoutやJob完了を待たない。
3. DispatcherがRP command queueへ送信する。RP Controllerは条件付きjournal更新と固定ClientTokenのEC2 launchを行い、再発見可能なaccepted checkpoint後にackする。起動API結果が不明なら同じattempt/token/parametersで再試行し、別VMを新規に作らない。
4. supervisorがsetup完了・process開始を報告した時点でCPを`running`へ更新する。result queueはattempt/generation、VM binding、artifact digestを持ち、CPが現行attemptと照合して終端状態とmetadataをtransactionで確定する。
5. VM終了、EBS削除、runner/token撤去を別のdurable cleanupとして追跡する。Run終端と物理cleanup完了は同義ではない。

EC2 ClientTokenは同一parametersと固定AZで使用する。AZを変更して同じtokenを送ることで別VMを作れるため、再試行時にplacementを変更しない。[EC2 idempotency](https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-idempotency.html)

### Delivery、fencing、障害復旧

- 短時間handlerの目標は60秒以内。queue visibility初期値120秒、heartbeat30秒、処理上限5分とする。これは導入時の設定であり、現行consumer defaultとは異なる。
- sequential consumerはまずbatch size 1とする。受信batchの後続messageが前message処理中にvisibility切れになる現行実装を、そのままbatch 10で配備しない。
- visibility更新失敗時は新しい副作用を中断し、receiptをackしない。DB Outbox/journal/ClientTokenから次回deliveryが再開する。ack失敗も同じ扱い。
- RP journalは`(tenant, run, attempt)`ごとの条件付きcreate/update、command revisionとcancel tombstoneを保存する。S3 `If-None-Match` / `If-Match`を必須にし、412/409時は再読込する。controllerのみ更新可能とし、無条件overwrite/deleteを許可しない。[S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
- journalとAWS APIはatomicではない。launch直後のcrashは固定ClientTokenとタグで再発見する。cancelがlaunchと競合したらtombstoneを維持して遅れて作られたVMも終了し、supervisorはcancel/expired attemptでコードを開始しない。
- heartbeat30秒、execution lease120秒を初期値とする。lease更新不可ならsupervisorはfail closedで子processを停止する。ただしlease失効だけで新attemptを起動しない。RPが旧VM終了を確認してからCPが新generationを許可する。
- stale resultは状態を上書きせず監査する。fencingは結果の一度だけの受理を保証する設計であり、任意外部副作用のexactly-onceを保証しない。VM終了が不明なら自動再実行せずrecovery待ちとする。
- DLQ redriveはRun/generation/deadline再検証が必須。生存VMがある可能性のまま新attemptに変換しない。

### Deadline、cancel、artifact

`timeout_at`は変更しない。1秒Runは起動前に期限切れとなり得る。24時間RunでもSQS receiptは短時間で解放する。supervisorは残り時間でprocessを制限し、CP reconciler/RP controllerもdeadline超過VMを終了する。時計同期と猶予はprocessの実行時間延長には使わない。

cancel APIはdesired stateとcancel Outboxを保存する。RPはprocess停止後VMを終了する。CPの`ExpireDue`や`cancelled`だけで実リソース停止済みと表示してはならない。`cleanup_pending/confirmed/failed`相当の独立状態と再試行・警報をTask 39で追加する。

redacted logsはsize limitとdigestを持ち、private S3へ保存する。VMに共有bucket全体への書込権限を渡さず、trusted broker経由または当該attempt限定の短命credentialでuploadする。resultをcommitする前にdigest/tenant/run prefixを検証する。upload成功・DB失敗はcontent-addressed objectを再利用し、未参照objectはretention後に回収する。

### Image、checkout、秘密情報、network

- AMIとOCI imageは固定ID/digest、scan/SBOM、署名検証、patch/rollback手順を持つ。runtime classはserver側でinstance/resource limitsへ写像する。
- GitHub App private keyはtrusted credential serviceだけがSecrets Managerから読む。scoped installation tokenは認可済み固定commitのcheckout時だけ使用し、repository/command/env/logに残さない。VMのrepository codeへは渡さない。
- trusted checkoutはhooks/filter/submodule/任意Git configを無効化し、SHA照合後のsourceと検証済みmodule cacheをread-onlyで渡す。networkなしGo testで依存不足なら安全に失敗する。repository指定のfetch/build scriptをhostで実行しない。
- 初期executorはnetwork none、non-root、cap-drop ALL、no-new-privileges、read-only root/source、bounded tmpfs/CPU/memory/pids。Docker socket、host env、IMDS、Secrets Manager、DSQLをcontainerから到達可能にしない。
- VM外向き通信はtrusted checkout/control/artifact経路に限定し、default denyとprivate endpoints/egress proxyを設計する。domain許可だけでcredential漏洩対策が完了するとは扱わない。
- rootless containerだけを強固なtenant隔離とは見なさない。kernel escapeでVMのsupervisor credentialが盗まれる残余リスクがあるため、VMはRunごとに破棄しcredentialの権限・寿命を当該attemptに限定する。

## Alternatives and consequences

| 基盤 | 評価・今回の判断 |
|---|---|
| 標準Lambda worker | 現行standard Lambdaのtimeout最大900秒で24時間execution不可。短時間dispatch/controllerには利用する。[Lambda timeout](https://docs.aws.amazon.com/lambda/latest/dg/configuration-timeout.html) |
| ECS/Fargate | task単位隔離と運用負担の低さは有利。privileged container非対応のため既存Podman-in-workerをそのまま移せない。直接Go testを動かす別executorは候補だが今回は採用しない。[ECS task security](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/security-tasks-containers.html) |
| ECS EC2共有worker | Podman/long jobを実装可能だが複数Tenantがhost kernelを共有する。特権worker共有は不採用。専用instance化すると今回の選択に近く、初期段階ではECS schedulerを追加しない |
| AWS Batch | queue/capacity/retry管理は有益。ただしEC2 hostのRun専有・破棄、attempt fencing、CPとの二重schedulerを別に設計する必要がある。規模拡大時に再評価 |
| 常駐専用host/Firecracker | microVM隔離とwarm起動は有益だがhost保守・guest image・snapshot/sanitization・capacity管理が増える。Workspace Runtime検討と分離して延期 |
| 使い捨てEC2 + rootless Podman | 既存adapterとの整合、nested container支援、Run単位VM隔離を優先して採用。cold boot、使い捨てdisk、1 Run 1 VMの費用、AMI保守と孤立VM回収が追加負担 |

価格の固定値はこのADRに含めない。導入時にregion/instance/quotaと起動時間を測定し、idle VMを残さない。warm poolを追加しても一度使ったhostを別Runへ戻さない。

## Staged validation and follow-up

1. Task 37/38: credential/checkoutのmock tests、tenant mismatch、固定SHA、secret非露出を検証。
2. Task 39: PostgreSQL + injected queues/provider/journalでdurable handoff、concurrent duplicate、cancel/launch競合、crash各点、stale result、artifact失敗、visibility失敗、24時間をfake clockで検証。既存Serviceの直接DB接続をRPへ持ち込まず、CP orchestrationとRP executorを分離する。
3. Task 40: default-off Terraform validate/plan、least-privilege policy/image tests、Podman local smoke。local Podmanはcommand policyの検証であってEC2/tenant隔離の証明ではない。常時起動Postgresは維持する。
4. 明示承認されたAWS環境でのみVM起動/中断/cleanup、egress/IMDS遮断、IAM越境拒否、crash recoveryと長時間leaseを検証。その後Task 42でPR→test→CheckのE2Eを行う。

現段階でIAM、journal、VM supervisor、cross-account transport、handler、cleanupは未実装。これらの故障試験と運用手順が揃うまで`HAKO_WORKLOAD_RUNS_ENABLED` / `enable_workload_run_creation`はdefault-offを維持する。本ADR承認は本番安全性の検証完了やAWS applyの承認ではない。
