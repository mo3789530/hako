# Fake Resource Controller と Runtime

## 目的と境界

`cmd/hako-fake-resource-controller`はResource PlaneのSQS commandを受信し、Fake RuntimeでWorkspace状態を模擬して、結果を結果Queueへ送ります。AWSのMicroVM、EFS、Networkなどは作成しません。Fake Runtimeの状態はプロセス内メモリだけにあり、再起動すると失われます。

Resource Plane ControllerはControl PlaneのPostgreSQL/Aurora DSQLへ接続しません。Control PlaneのTransactional Outboxからcommandを受け取り、独立したresult queueへ結果を報告します。Control Plane側の[`cmd/hako-result-consumer`](operation-results.md)が結果を消費し、Operation/Observed Stateを永続化します。両プロセスとDispatcherを接続して起動するとFake Operationは完了します。

```text
Control Plane Outbox -> command SQS -> Fake Resource Controller
                                             | Fake Runtime
                                             v
                                      result SQS
                                             |
                             Control Plane Result Consumer
```

## Command / Result

ControllerはJSON envelope `schema_version: 1`を受け付けます。必須情報はOperation、Tenant、Workspace、Resource Plane ID、Operation type、作成時刻です。未知フィールド、未対応version/type、別Resource Plane宛のcommandは拒否します。結果もschema version 1で、Operation/Tenant/Workspace/Resource Plane ID、type、terminal status、Observed State、完了時刻を含みます。

対応Operationは`ensure_running`、`resume`、`suspend`、`delete`です。Fake RuntimeはそれぞれObserved Stateを`running`、`running`、`suspended`、`deleted`へ設定します。Operation IDを冪等キーとして結果をキャッシュし、同じOperationの再配送では状態変更を繰り返さず同じ結果を返します。同じOperation IDを異なるWorkspace/typeで再利用するとエラーになります。

実行成功時は`status: succeeded`とObserved State、実行失敗時は`status: failed`、`observed_state: failed`、`error_code: runtime_error`を含む結果を送信します。Result Consumerは保存済みOperation identityと照合して状態を反映し、重複・遅延したterminal結果をno-opとしてackします。Workspace generation/fencingは未実装です。

## 再配送とSQS

Workerは最大10件のbatchをlong pollし、batch内のmessageを並行処理します。各処理中messageのvisibilityをtimeoutの半分ごとに延長し、実行timeout（既定15分）を超えたRuntime呼び出しは`operation_timeout`結果として報告します。timeout値は`HAKO_OPERATION_TIMEOUT`（Go duration、例`15m`）、初回visibilityは`HAKO_SQS_VISIBILITY_TIMEOUT`（秒、1〜43200）で設定できます。

結果Queueへの送信が成功してからcommand messageを削除します。処理または結果送信に失敗したmessageは削除せず、`ApproximateReceiveCount`に基づく指数backoff（既定1秒から最大1分）をSQS visibilityへ設定します。したがってcommand deliveryと結果通知はat-least-onceであり、Runtimeの冪等性とresult consumerのCASが必要です。

不正commandも現在は削除せず再配送対象になります。Resource Plane queue/DLQのTerraform定義は実装済みで、command/resultごとのDLQ、redrive policy、DLQ redrive allow policyを設定し、既定の`maxReceiveCount`は5です。ただしLambdaやevent source mappingへの接続・AWS apply・DLQ監視/redrive手順は未実装です。詳細は[Resource Plane queues and DLQ](resource-plane-queues-and-dlq.md)を参照してください。Control PlaneのReconcilerは30分（`HAKO_RECONCILER_OPERATION_TIMEOUT`で変更可能）更新のないpending/running Operationをfailedにし、`operation.timed_out` eventを記録します。ただしこれはmessageやRuntime processをcancelせず、Observed Stateも変更しません。late command/resultや部分作成リソースの補償にはRuntime-side fencing/cleanupが別途必要です。仕様は[Workspace Reconciler](reconciler.md#stale-operation-timeout)を参照してください。

## ローカル実行

AWS SDK default credential chainを使用します。次の変数を設定し、command Queueと結果Queueの双方に必要なSQS権限を付与してください。Terraform moduleのqueue URL outputsをそれぞれ設定できます。

```sh
export HAKO_RESOURCE_PLANE_ID=rp-local
export HAKO_RESOURCE_PLANE_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/hako-rp-local
export HAKO_OPERATION_RESULT_QUEUE_URL=https://sqs.<region>.amazonaws.com/<account>/hako-operation-results
export AWS_REGION=<region>
go run ./cmd/hako-fake-resource-controller
```

command Queueには`sqs:ReceiveMessage`、`sqs:DeleteMessage`、`sqs:ChangeMessageVisibility`、結果Queueには`sqs:SendMessage`が必要です。既定値は最大10件、20秒long poll、60秒visibility timeout、15分Runtime execution timeout、1秒〜1分のretry backoffです。Queue、DLQ、RedrivePolicy、Queue policy、Resource Controller IAM roleはTerraformで定義済みですが、AWSへは適用されていません。

Fake Runtime/ControllerのロジックとWorkerの「結果報告前にackしない」条件は`go test ./internal/runtime/fake ./internal/resourcecontroller`で検証できます。APIからの作成・起動・停止・削除をFake Runtimeまで通すPostgreSQL integration/E2Eは別タスクです。
