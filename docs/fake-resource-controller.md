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

Controllerは旧キュー互換のschema version 1と、新規writerが使うschema version 2を受け付けます。Version 2は正の`workspace_revision`を必須とし、結果も同じschema version/revisionをechoします。未知フィールド、未対応version/type、別Resource Plane宛のcommand、不正なrevisionは拒否します。

対応Operationは`ensure_running`、`resume`、`suspend`、`delete`です。Fake RuntimeはそれぞれObserved Stateを`running`、`running`、`suspended`、`deleted`へ設定します。Operation IDを冪等キーとして結果をキャッシュし、同じOperationの再配送では状態変更を繰り返さず同じ結果を返します。同じOperation IDを異なるWorkspace/typeで再利用するとエラーになります。

Fake RuntimeはOperation IDの結果を冪等に返し、Workspace revision watermarkより古いrevisionや、同じrevisionで異なるOperationを拒否します。Controllerは成功時にObserved State、失敗時に`observed_state: failed`と`error_code`を含むresultを送信します。Result Consumerも現在revisionと照合して古いresultを適用しません。Fake watermarkはプロセス内のみです。実Runtimeはresource bindingと削除tombstoneを含む耐久メタデータでrevisionを比較・更新してからAWS side effectを行う必要があり、その実装は未完了です。

## 再配送とSQS

Workerは最大10件のbatchをlong pollし、batch内のmessageを並行処理します。各処理中messageのvisibilityをtimeoutの半分ごとに延長し、実行timeout（既定15分）を超えたRuntime呼び出しは`operation_timeout`結果として報告します。timeout値は`HAKO_OPERATION_TIMEOUT`（Go duration、例`15m`）、未実行commandの最大ageは`HAKO_COMMAND_MAX_AGE`（既定`30m`）、初回visibilityは`HAKO_SQS_VISIBILITY_TIMEOUT`（秒、1〜43200）で設定できます。

Controllerは`created_at`が現在時刻より5分を超えて未来ならcommandを拒否し、Queue retry/DLQ対象にします。最大ageを超えているcommandはRuntimeを呼ばず、`operation_expired` failed resultを報告します。結果送信に成功すればWorkerはそのcommandをackし、pending Operationは通常のFailure cooldown後にReconcilerが現在のDesired Stateへ収束させます。結果送信に失敗した場合はcommandを残して再試行します。`HAKO_COMMAND_MAX_AGE`はControl Planeの`HAKO_RECONCILER_OPERATION_TIMEOUT`以下に設定してください（両方の既定値は30分）。

結果Queueへの送信が成功してからcommand messageを削除します。処理または結果送信に失敗したmessageは削除せず、`ApproximateReceiveCount`に基づく指数backoff（既定1秒から最大1分）をSQS visibilityへ設定します。したがってcommand deliveryと結果通知はat-least-onceであり、Runtimeの冪等性とversion-2 Workspace revision fenceが必要です。

不正commandも現在は削除せず再配送対象になります。Resource Plane queue/DLQのTerraform定義は実装済みで、command/resultごとのDLQ、redrive policy、DLQ redrive allow policy、DLQ/backlog/age alarmを設定し、既定の`maxReceiveCount`は5です。ただしAWS applyとDLQ確認/redrive手順は未実装です。詳細は[Resource Plane queues and DLQ](resource-plane-queues-and-dlq.md)を参照してください。Control PlaneのReconcilerは30分（`HAKO_RECONCILER_OPERATION_TIMEOUT`で変更可能）更新のないpending/running Operationをfailedにし、`operation.timed_out` eventを記録します。これはmessageや既に実行中のRuntime processをcancelしません。Version 2 fenceはsuperseded command/resultを拒否しますが、実Runtime側のdurable watermarkと部分作成リソースの補償cleanupは別途必要です。仕様は[Workspace Reconciler](reconciler.md#stale-operation-timeout)を参照してください。

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
