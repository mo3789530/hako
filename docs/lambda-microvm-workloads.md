# Lambda MicroVM Workload Controller

HakoのWorkload基盤はAWS Lambda MicroVMs。`internal/workloadcontroller`は条件付きRP journal、trusted specification resolver、MicroVM provider、result publisherを注入して使う。RPからDSQLへ接続しない。

## 実装済みの経路（Tasks 63–65）

1. `Ensure`でidentity-only commandを受理し、固定commit/source/image digest・deadline・placementの仕様metadataとハッシュをCASで保存する。Secret値は含めない。cleanupはこのmetadataで回復し、Repository認可サービスの存続に依存しない。
2. `LambdaMicroVM`が公式SDKの`RunMicrovm`へ固定ClientToken、固定image/version、固定network connector、固定maximum durationを渡す。応答不明時も同じparametersを使う。
3. MicroVM IDをCASで保存する。`GetMicrovm`のstateだけでJob開始済みと判断せず、注入した`AgentRunner.Start`で準備と一度だけのJob開始を確認する。SDK stateはeventually consistent。
4. `Poll`がagentのJob status/resultを取得し、Tenant/Run/attempt/generation/commit、redacted logのサイズ・digest・attempt別S3 prefixを検証する。結果metadataをjournalへ保存してから送信し、送信後checkpoint失敗時は同じ結果を再送する。
5. `Cancel`はjournalへtombstoneとlogical terminal stateを保存してから停止する。`TerminateMicrovm`成功だけではcleanup完了とせず、`GetMicrovm`の`TERMINATED`を確認する。停止/cleanup失敗は再試行できる。

generation > 1のSpecificationには直前attemptのidentityが必要。同じTenant/Run/Resource Planeの直前generationについてcleanup確認済みのjournalを読み取れなければreplacementを起動しない。

CPの`RequestCancel`はdesired stateとcurrent attempt向け`workload_attempt.cancel` Outboxを同じtransactionに保存する。Dispatcherは固定Resource Plane queueへ配送し、`HakoEventType`でdispatch/cancelを識別する。再度のcancelでは追加Outboxを作らない。attempt作成前のcancelはschedulerが検出する。既存DB roleにはbootstrap templateの`SELECT ON workload_run_attempts`を適用する必要がある。

## AWSの仕様とHakoの制約

- **1 MicroVMの最大存続時間は8時間（28,800秒）、ARM64のみ。** 汎用Run APIのdeadline上限24時間と異なる。AWS adapterは元のrequest intervalが8時間を超えるSpecificationを起動前に拒否する。fake-clockの24時間テストはCP契約の検証であり、AWSで24時間実行できる証明ではない。
- deadlineは作成時基準の絶対時刻。AWS maximum durationはretryで変化させず、agentも絶対deadlineを守り、controllerが期限切れを停止する。期限延長・自動Job再実行・暗黙の別基盤への変更はしない。
- Jobのidle policyはauto-resume無効、max idleを固定maximum duration以上（最小60秒）に設定する。proxyへのinbound通信量で判定される標準idle policyにより、無通信の長時間Jobが途中suspendすることを避ける。
- Lambda MicroVMsの標準egressはInternet。承認済みVPC egress connectorを必須とし、VPC側でdefault-denyと必要経路だけを設定する。connector ARN指定だけでネットワーク隔離済みとは扱わない。
- 初期adapterはcommercial `aws` partition、固定image ARN/numeric version/digest、明示的なAWS-managed `ALL_INGRESS` connectorを扱う。接続はHTTPS、agent portだけの短命認証tokenを用いるAgentRunner実装が必要。公開shell connectorは追加しない。
- `ExecutionRoleArn`は指定しない。取得したMicroVMに想定外のexecution roleがあればagentを開始しない。GitHub App private keyやSecret値を`runHookPayload`へ渡さない。
- `/run` hookはtrusted agentの初期化だけを行う。unknown launchのcancel時に、同じClientTokenでIDを回復する`RunMicrovm`が新しいVMを作る可能性があるため、hookでrepository codeを実行してはならない。
- Docker/containerd/Podman、EFS、suspend/resume、imageの署名・source搬送の実証はTask 61/43–45に残る。一般Lambda functionのcontainer image配備とは異なるAPIを使う。

## 未接続の本番構成

controller、公式SDK adapter、CP cancel Outbox、result replayのlibraryは実装済み。trusted resolverの実service、認証付きAgentRunner、SQS consumer/result publisher配備、journal recovery scheduler、image/network/IAM provisioningは未接続。durable journal保存だけでmessageをackするworkerを配備しない。

実AWSのclient-token再試行・保持期間、並行launch/terminate、agent起動、connectorのegress制限、quota/費用、cleanup/孤立VMの回収とalarmをTask 40で検証するまでRun作成はdefault-off。今回AWS APIを実行してリソースを作成・変更していない。

## 検証

Tasks 63–65のunit/race testsはfake journal/providerとSDK stubを使う。PostgreSQL integrationはCP cancellation transactionのrollback/duplicate、cancelがdispatchより先に届くケース、RP resultのCP commit後ack喪失・duplicate replay・artifact metadata一度だけ保存を検証する。常時起動DBは停止しない。

```bash
make test-workload-runtime
make test-workload-runtime-integration
```

integrationは既定のローカルPostgresまたは`HAKO_TEST_DATABASE_URL`を使い、テストごとのschemaを作成・回収する。実AWSリソースやDBコンテナのlifecycleは操作しない。

## 公式資料

- [Lambda MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
- [Quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html)
- [RunMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_RunMicrovm.html)
- [GetMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_GetMicrovm.html)
- [TerminateMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_TerminateMicrovm.html)
- [Networking](https://docs.aws.amazon.com/lambda/latest/dg/microvms-networking.html)
- [Security/IAM](https://docs.aws.amazon.com/lambda/latest/dg/microvms-security.html)
- [IdlePolicy](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_IdlePolicy.html)
