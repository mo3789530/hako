# Operation状態遷移とイベント履歴

## 永続化境界

Operationの状態変更は`operations.Transition`で行います。1回のtransactionで次をまとめて保存します。

1. `operations.status`、`updated_at`、`error_code`をCompare-and-Swapで更新。
2. `operation_events`へ次のsequenceを採番してイベントを追加。
3. 必要な場合は対応Workspaceの`workspace_status.observed_state`を更新。

どれか一つでも失敗した場合はtransaction全体をrollbackします。初期`operation.created` eventはWorkspace作成transactionがsequence 1として保存し、その後は最大sequence + 1を採番します。履歴は`ListEvents`でsequence順に読み出せます。

## 状態遷移

許可する遷移は限定します。

```text
pending -> running
pending -> cancelled
pending -> failed         timeout only
running -> pending       retry待ち
running -> succeeded
running -> failed
running -> cancelled
```

`running`への遷移ごとに`attempt`を1増やします。`succeeded`、`failed`、`cancelled`は終端状態です。再実行が必要なら既存Operationを終端から戻さず、新しいOperationを作成します。

Reconcilerは`HAKO_RECONCILER_OPERATION_TIMEOUT`を超えたpending/running Operationを`operation_timeout`でfailedにし、`operation.timed_out` eventを同一transactionへ追加します。timeoutはResource Planeの実行停止を意味しないため、Observed Stateは変更しません。遅着resultはterminal Operationへの重複resultとして無視されます。詳しくは[Workspace Reconciler](reconciler.md#stale-operation-timeout)を参照してください。

呼び出し側は`ExpectedStatus`を指定します。現在の状態と異なる場合は`ErrTransitionConflict`を返し、イベントを追加しません。重複・遅延したResource Plane結果は、状態を再読込してから扱う必要があります。Aurora DSQLのOCC競合は`transaction.Within`がtransaction全体を再実行します。

Resource Planeのterminal resultは[`operations.ApplyResult`](operation-results.md)を通します。Operation/Tenant/Workspace/Resource Plane/type identityを照合し、pendingならrunningへの開始eventとterminal event、Observed State、削除quota解放までを1 transactionで記録します。すでにterminalのOperationへの重複・遅延resultはno-opとして扱い、現状態を上書きしません。

## Workspace observed state

Operationの状態とWorkspaceの観測状態は別概念です。Transition呼び出しでは、Resource Plane等から確認した観測状態だけを任意に渡します。Desired Stateはこの処理では変更しません。Operation状態、event履歴、observed stateは同一transactionで確定します。

`workspace_status.reconcile_revision`は複数Reconciler間の同一状態claimを防ぐために使いますが、Resource Plane結果をfenceするWorkspace generationではありません。古いOperation結果が新しいDesired Stateを上書きしないためのgeneration/fencing token、およびResource ControllerでのOperation順序制御は未実装です。

Event payloadは省略可能なJSONで、`event_type`は非空の文字列です。イベントは追記専用として扱い、状態変更の監査履歴と再構築可能な進捗記録に使います。
