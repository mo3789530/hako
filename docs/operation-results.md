# Operation Result Consumer

## データ経路

Resource PlaneはControl PlaneのDBへ接続せず、実行結果をOperation result SQSへ送ります。`cmd/hako-result-consumer`が結果を受け、OperationとWorkspace状態へ反映します。

```text
API -> DB transaction (Workspace + Operation + Outbox)
  -> Outbox Dispatcher -> Resource Plane command SQS
  -> Fake/Resource Controller -> Operation result SQS
  -> Result Consumer -> DB transaction (Operation + event + Observed State)
```

Result envelopeはschema version 1で、Operation/Tenant/Workspace/Resource Plane ID、Operation type、terminal status、Observed State、完了時刻を含みます。ConsumerはOperationに保存されたtenant、workspace、placement、typeと照合し、不一致を拒否します。

## 状態反映と重複

成功結果のObserved StateはOperation typeに対応する値でなければなりません（ensure/resume=`running`、suspend=`suspended`、delete=`deleted`）。失敗結果は`failed` Observed Stateとerror codeを要求します。

DB transactionでは、pending Operationをrunningへ進めて`operation.started` eventを追加し、terminal statusと`operation.succeeded`/`operation.failed` event、Observed Stateを記録します。すでにrunningならterminal transitionだけ行います。削除成功時はObserved State更新とTenant quota slot解放も同じtransactionです。

結果queueはat-least-onceです。DB commit後にSQS ackが失敗すれば同じ結果が再配送されます。すでにterminalのOperationに届いた重複・遅延結果は状態を変更せずackします。Identity不一致、未知Operation、不正payload、DB障害ではackせず、Queueのredrive policyがあれば設定回数後にDLQへ移ります。FIFOの重複排除だけを正しさの根拠にはしません。

## 起動

Control Plane側で次を設定します。

```sh
export HAKO_OPERATION_RESULT_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/hako-operation-results
export AWS_REGION=<region>
# local PostgreSQL:
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
# または本番Aurora DSQL:
# export HAKO_DSQL_HOST='<cluster-id>.dsql.<region>.on.aws'
go run ./cmd/hako-result-consumer
```

Consumer roleには結果Queueへの`sqs:ReceiveMessage`と`sqs:DeleteMessage`、Control Plane DBへの接続権限が必要です。Resource Plane側には結果Queueへの`sqs:SendMessage`を付与します。Queue、cross-account resource policy、redrive/DLQのTerraformはまだありません。

## 現在の安全境界

このConsumerはOperation IDの結果をCAS/terminal-state guard付きで反映しますが、Workspace generationを含むfencing tokenはまだありません。AWS実リソース作成の部分成功補償やCleanup、DLQの再処理runbookも未実装です。Fake RuntimeとのAPI lifecycle integration testはPostgreSQL上でOutboxにcommitしたcommandを直接Fake Controllerへ渡し、result transportはin-memory queueで模擬します。署名済みsynthetic Cognito tokenを使い、AWS SQS/Cognito実環境なしで実行できます。実SQSプロセス間を通すAWS E2Eは別タスクです。
