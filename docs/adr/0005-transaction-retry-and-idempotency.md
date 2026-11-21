# ADR 0005: OCC retry、transaction境界、冪等性

- Status: Accepted
- Date: 2026-09-23

## Context

Aurora DSQLはOCCで競合を検出し、transactionのcommit時に失敗を返す。ローカルPostgreSQLでもserializable failureが起き得る。Retry時に外部副作用を繰り返すと、AWS操作やQueue配信が重複する。

## Decision

- DB transaction全体を新しいtransactionで再実行する。Aurora DSQLの`OC000`、`OC001`、`40001`とPostgreSQLの`40P01`のみをbounded exponential backoff + jitterでretryする。
- 初期値は最大5回のretry、50ms開始、1秒上限とする。context cancellationは待機を即時終了する。
- `transaction.Within`のcallbackはDB操作だけに限定する。AWS API、SQS送信、ファイル操作、通知などはcallbackから呼ばない。
- Workspace、Operation、Operation Event、Outbox Eventの作成と状態変更は同じDB transactionに含める。Queue配送などの副作用はcommit後にOutbox Dispatcherが行う。
- Idempotency keyはTenant内で一意とし、1〜128 byteの表示可能ASCIIを受け入れる。CLI/API clientは論理要求ごとに新しいUUIDv4を生成し、HTTP retryでは同じkeyを再利用する。
- DBにはkeyとnormalized requestのSHA-256 fingerprintを保存し、keyは`(tenant_id, idempotency_key)`でuniqueにする。同じkey・同じ要求なら既存Operationを返し、異なる要求ならconflictとして扱う。
- Fingerprintにはtenant、operation type、正規化済み要求payloadを含める。Workspace作成のpayloadにはOwner IDとWorkspace設定を含める。サーバーが生成するID、時刻、Schedulerが選んだResource Planeなどの内部値は含めない。

## Consequences

- Retry callbackは複数回実行される前提で書く必要がある。
- 同じkeyの衝突時にpayload全体を保存せず要求同一性を判定できる。
- APIは冪等性衝突をHTTP 409相当の安定したエラーへ変換する。
- AWSやSQSへの実処理はTransactional Outbox実装まで開始しない。
