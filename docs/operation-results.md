# Operation Result Consumer

## データ経路

Resource PlaneはControl PlaneのDBへ接続せず、実行結果をOperation result SQSへ送ります。`cmd/hako-result-consumer`が結果を受け、OperationとWorkspace状態へ反映します。

```text
API -> DB transaction (Workspace + Operation + Outbox)
  -> Outbox Dispatcher -> Resource Plane command SQS
  -> Fake/Resource Controller -> Operation result SQS
  -> Result Consumer -> DB transaction (Operation + event + Observed State)
```

新しいResult envelopeはschema version 2で、Operation/Tenant/Workspace/Resource Plane ID、Workspace revision、Operation type、terminal status、Observed State、完了時刻を含みます。ConsumerはOperation identityとWorkspaceの現在revisionを照合し、不一致・古いrevisionを適用しません。旧schema version 1のキュー内resultは移行互換として読み取りますがrevision fenceはありません。

## 状態反映と重複

成功結果のObserved StateはOperation typeに対応する値でなければなりません（ensure/resume=`running`、suspend=`suspended`、delete=`deleted`）。失敗結果は`failed` Observed Stateとerror codeを要求します。

DB transactionでは、pending Operationをrunningへ進めて`operation.started` eventを追加し、terminal statusと`operation.succeeded`/`operation.failed` event、Observed Stateを記録します。すでにrunningならterminal transitionだけ行います。削除成功時はObserved State更新とTenant quota slot解放も同じtransactionです。

結果queueはat-least-onceです。DB commit後にSQS ackが失敗すれば同じ結果が再配送されます。すでにterminalのOperationに届いた重複・遅延結果は状態を変更せずackします。Identity不一致、未知Operation、不正payload、DB障害ではackせず、Queueのredrive policyがあれば設定回数後にDLQへ移ります。FIFOの重複排除だけを正しさの根拠にはしません。

## 起動

Control Plane側では、複数のResource Planeを使う場合、Dispatcherと同じ登録manifestをResult Consumerへ渡します。

```sh
export HAKO_RESOURCE_PLANE_MANIFEST=./config/resource-planes.json
# manifest内の各result queueをRegionごとのpoll workerが並行処理する。
export AWS_REGION=<control-plane-default-region>
# local PostgreSQL:
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
# または本番Aurora DSQL:
# export HAKO_DSQL_HOST='<cluster-id>.dsql.<region>.on.aws'
go run ./cmd/hako-result-consumer
```

単一Queueまたは移行時は`HAKO_OPERATION_RESULT_QUEUE_URL`、複数Queueの手動設定は`HAKO_OPERATION_RESULT_QUEUE_URLS`（Plane IDからQueue URLへのJSON map）も利用できます。manifest、単一Queue、Queue mapは同時指定できません。manifest利用時はQueueごとにRegion-specific SQS clientを作成し、各queueを独立long pollします。Consumer roleには各結果Queueへの`sqs:ReceiveMessage`と`sqs:DeleteMessage`、Control Plane DBへの接続権限が必要です。Resource Plane側には結果Queueへの`sqs:SendMessage`を付与します。Queue、cross-account resource policy、redrive/DLQはTerraformに定義済みですが、AWSには適用していません。

## 現在の安全境界

Version 2のresultにはmonotonicなWorkspace revisionを含めます。現在revisionと一致しない古いresultはno-opとしてackします。Schema version 1は既存Queue drain期間のために引き続き受け付けます。AWS実リソース作成の部分成功補償やCleanup、DLQの再処理runbookも未実装です。Fake RuntimeとのAPI lifecycle integration testはPostgreSQL上でOutboxにcommitしたcommandを直接Fake Controllerへ渡し、result transportはin-memory queueで模擬します。実SQSプロセス間を通すAWS E2Eは別タスクです。
