# 開発環境

## 前提

- Go 1.27以降
- Terraform 1.12.1
- Make
- Dev Containerを使う場合はDocker対応のDev Container環境

Control Plane Terraform rootはHTTP API、Cognito JWT authorizer、`hako/api` scope付きdefault route、API Lambda、Aurora DSQL、IAM Roleを定義します。Cognito User Poolは既存のものを入力します。AWSアカウントや認証情報なしでformatとvalidateを実行できます。`plan`には実際のCognito User Pool issuer URLとUser Pool ID、および`make build-api-lambda`で生成したpackageが必要です。app client IDはTerraform outputから取得できます。User Pool domainは別途用意してください。別のCLI callback URLが必要な場合は`cognito_cli_callback_urls`を指定し、同じURLをCLIにも設定してください。

## ローカル確認

```sh
make fmt
make lint
make test
make test-integration-local
make test-coverage HAKO_TEST_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
make terraform-fmt
make terraform-validate
```

`terraform-validate`は`infra/terraform/environments/dev`以下のControl PlaneとResource Planeを個別に初期化・検証します。実際のAWSリソース追加時に、各アカウントと環境のremote state設定を追加します。

## PostgreSQL統合テスト

`make test-integration-local`は常時起動の開発用PostgreSQL 18を使い、migration、Cognito subjectとHako Userの対応付け、Tenant membership拒否、Workspace作成とOutboxの原子性、冪等再送、Operation状態遷移、イベント順序、Observed State更新、Tenantごと最大10件の同時作成quotaと削除後のslot再利用を検証します。CLI suiteはローカルOAuth/JWKSと署名済みテストtokenでPKCE login、Fake Runtime、結果Consumerを使い、作成・状態確認・停止・再開・削除に加えて、重複command、result ack失敗後の再配送、非同期Desired/Observed State差、他Member Workspaceへの認可拒否を通します。OS keyringや実AWS/Cognitoは不要です。DBには専用のランダムschemaを作成し、テスト終了時にそのschemaだけを削除します。データベースやvolumeは削除しません。テスト境界は[Authenticated CLI Lifecycle E2E](cli-lifecycle-e2e.md)を参照してください。

別の使い捨てPostgreSQLを使う場合は、`HAKO_TEST_DATABASE_URL`を設定して`make test-integration`を実行します。接続先のDBユーザーにはschema作成・削除権限が必要です。integration testはbuild tag付きのため、通常の`go test ./...`では実行されません。GitHub ActionsではPostgreSQL serviceを起動して全packageのintegration testを実行します。

coverageは`make test-coverage HAKO_TEST_DATABASE_URL=...`で測定します。これはintegration build tagを含めて全Go packageをinstrumentし、`coverage.out`と関数別reportを生成します。通常の単体test coverageとDB-backed coverageを区別し、低coverageのpackage/branchから回帰テストを追加してください。coverage percentageは機械的な合格目標にせず、認可、冪等性、transaction境界、retry、失敗cleanupなど重要な分岐がテストされているかをレビューします。

このsuiteは通常PostgreSQLでのschema/SQL/Control Plane store連携を検証します。Aurora DSQLのIAM認証、OCC競合挙動、AWS API、HTTP API、Resource Plane配送を実環境で検証するAWS E2E suiteとは別です。

## Aurora DSQL

Migration CLIはAWS SDKのdefault credential chainを使い、Aurora DSQL IAM tokenを自動生成するAWS公式pgx connectorで接続します。実行時は少なくとも次の値を設定します。

```sh
export HAKO_DSQL_HOST='<cluster-id>.dsql.<region>.on.aws'
export AWS_REGION='<region>'
# Migration IAM role uses dsql:DbConnectAdmin. HAKO_DSQL_USER defaults to admin.
make migrate
```

SecretやDB passwordは環境変数に設定しません。Migrationは`internal/store/dsql/migrations/`の連番SQLを適用します。Aurora DSQLの制約に合わせ、各ファイルは単一の冪等DDL文とし、DDL成功後にmigration ledgerを別のDML文で更新します。複数のmigration runnerを同時に実行せず、デプロイで1つだけ起動してください。

## Local PostgreSQL

開発中は通常のPostgreSQLをPodmanで常時起動し、同じMigrationを適用します。`hako-dev-postgres`は`unless-stopped` restart policyと`hako-postgres-data` named volumeを使います。タスク間でDBを停止しないでください。既存の開発DBがある場合は再作成せず、そのDBを使います。

```sh
podman ps --filter name=hako-dev-postgres
podman exec hako-dev-postgres pg_isready -U hako -d hako
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
make migrate
```

新しい環境でまだDB containerがない場合のみ、次で一度作成します。

```sh
podman run --detach --name hako-dev-postgres --restart unless-stopped \
  --env POSTGRES_USER=hako --env POSTGRES_PASSWORD=local-dev-only \
  --env POSTGRES_DB=hako -p 127.0.0.1:5432:5432 \
  --volume hako-postgres-data:/var/lib/postgresql docker.io/library/postgres:18
```

`HAKO_DATABASE_URL`を指定した場合は通常のPostgreSQLを使います。未指定の場合はAurora DSQL接続設定を使います。両方を同時に設定すると誤接続防止のためエラーになります。開発DBはlocalhostだけにbindし、認証情報はローカル開発用です。

## CLI Cognito login

`hako login`の動作確認にはCognito User Pool domainが必要です。public app clientはControl Plane Terraform rootが作成し、client IDは`tofu -chdir=infra/terraform/environments/dev/control-plane output -raw cognito_app_client_id`で取得します。Authorization Code grantと`openid email profile hako/api` scopes、loopback callback、keyringのセキュリティ前提は[Cognito CLI login](cognito-cli-login.md)を参照してください。開発環境のLinuxではSecret Service (GNOME Keyring/KDE Wallet)が必要です。

APIのCognito access token検証とAPI Gateway HTTP API authorizerの設定・未実装範囲は[Cognito API認証](cognito-api-auth.md)に記載しています。dev rootでplanするには`cognito_issuer_url`と`cognito_user_pool_id`を`-var`または未コミットのtfvarsから渡してください。Cognito `sub`のHako User対応付けとTenant membership guardは[Tenant identity/RBAC](tenant-identity-and-rbac.md)を参照してください。

API GatewayからGo Lambdaへのproxy route、Gateway側`hako/api` scope、Go binary packagingは[API Gateway Lambda](api-gateway-lambda.md)を参照してください。Control Plane Terraformは`make build-api-lambda`のartifactからLambda Function/Roleを作成します。Aurora DSQLの`hako_api` DB Role/IAM mappingはmigration adminで一度だけbootstrapする必要があります。

Go APIはEcho v5で実装しており、`cmd/hako-api`から起動できます。API起動時にはDB接続が必要です。ローカルでは常時起動のPodman PostgreSQLにmigrationを適用し、`HAKO_DATABASE_URL`を設定します。本番Aurora DSQLでは`HAKO_DSQL_HOST`と`AWS_REGION`、実行IAM Roleを使用します。両方の接続方式を同時に指定すると起動を拒否します。

`GET /healthz`は認証不要の最小liveness probeです。`GET /v1/health`には有効なCognito access tokenと`hako/api` scopeが必要です。`GET /v1/tenants/{tenant_id}/membership`は検証済みCognito `sub`をHako Userへ対応付け、Tenant Membershipを強制します。CLIの`hako api health`と`hako tenant membership <tenant-id>`はOS credential storeからaccess tokenを読み、`HAKO_API_URL`へ送ります。local HTTPはloopbackだけ許可し、期限切れtokenは自動refreshせず再ログインを案内します。認可の詳細は[Cognito CLI login](cognito-cli-login.md)と[Tenant identity/RBAC](tenant-identity-and-rbac.md)を参照してください。

Workspaceは`POST /v1/tenants/{tenant_id}/workspaces`から非同期作成でき、CLIは`hako create <tenant-id> <workspace-name>`を使います。作成にはTenant membership、`Idempotency-Key`、`HAKO_DEFAULT_WORKSPACE_IMAGE`が必要です。Schedulerはactiveかつ`microvm` capabilityを持つResource Planeから選び、healthyを優先、degradedをfallback、unhealthyを除外します。Tenant Owner/Adminは`GET`/`PUT /v1/tenants/{tenant_id}/placement-policy`または`hako tenant placement-policy get|set`でTenant別の追加制約を管理できます。Resource PlaneのWorkspace/CPU/RAM上限は`resource_plane_capacities`で任意設定でき、CPU/RAM制限を使う場合は`runtime_class_resources`にRuntime Classごとの要求量を登録します。上限超過Planeは候補から外れます。Runtime Classは`HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS`（既定`standard`）です。候補がなければHTTP 503 `resource_plane_unavailable`を返し、DBに作成レコードやQuota slotを残しません。11件目はHTTP 409 `workspace_quota_exceeded`として返ります。Schedulerの選択規則は[Scheduler Placement](scheduler-placement.md)、capacity設定は[Resource Plane Capacity](resource-plane-capacity.md)、health更新と前提は[Resource Plane Health](resource-plane-health.md)を参照してください。

Workspace一覧・取得は`GET /v1/tenants/{tenant_id}/workspaces`と`GET /v1/tenants/{tenant_id}/workspaces/{workspace_id}`です。Memberには自分がOwnerのWorkspaceだけを表示し、Owner/AdminにはTenant内すべてを表示します。一覧は既定20件、`limit`最大100、`offset`最大1,000,000で、`hako list <tenant-id> [limit] [offset]`から利用できます。単体取得は`hako get <tenant-id> <workspace-id>`です。詳細な応答・権限仕様は[Workspace上限と閲覧権限](workspace-limits-and-visibility.md)を参照してください。

停止・再開・削除は`POST /v1/tenants/{tenant_id}/workspaces/{workspace_id}/actions`へ`suspend`、`resume`、`delete`を送る非同期Operationです。CLIは`hako suspend|resume|delete <tenant-id> <workspace-id>`を提供し、通信失敗時に再試行できるようIdempotency-Keyを表示します。APIの`202 Accepted`は要求の永続化を示します。Fake Runtimeを接続するとOperation結果とObserved StateをDBへ反映できますが、実MicroVMの停止・再開・削除は未実装です。QuotaはObserved Stateが`deleted`になるまで保持されます。状態条件、認可、現状の境界は[Workspace lifecycle API](workspace-lifecycle-api.md)を参照してください。

Transactional Outboxはローカルでは`go run ./cmd/hako-dispatcher`がpollし、AWS環境ではopt-inの`cmd/hako-dispatcher-lambda`をEventBridgeで毎分起動できます。複数Resource Plane環境ではDispatcher/Result Consumerのprocess版に`HAKO_RESOURCE_PLANE_MANIFEST=config/resource-planes.json`、Dispatcher Lambdaには`HAKO_RESOURCE_PLANE_MANIFEST_JSON`を設定します。DispatcherはQueueごとにAWS Regionを設定したSQS clientでpublishし、Result ConsumerはQueueごとにlong poll workerを起動します。Dispatcherには各command Queueの`sqs:SendMessage`、Result Consumerには各result Queueのreceive/delete権限が必要です。process版はDispatcherの`HAKO_RESOURCE_PLANE_QUEUE_URLS`、Result Consumerの`HAKO_OPERATION_RESULT_QUEUE_URL`でも利用できます。Lambdaのartifact、Terraform適用順、IAM/DSQL前提は[Outbox Dispatcher](outbox-dispatcher.md)を参照してください。commandをFake Runtimeで処理する場合は`go run ./cmd/hako-fake-resource-controller`を別プロセスで起動し、対象Planeのcommand/result Queue URLとResource Plane IDを設定します。Fake Controllerは実AWSリソースを操作せず、Control PlaneのDBにも接続しません。必要IAM権限や結果契約は[Fake Resource Controller](fake-resource-controller.md)と[Operation Result Consumer](operation-results.md)を参照してください。

Workspace Reconcilerは`go run ./cmd/hako-reconciler`で起動します。Desired/Observed Stateがずれ、pending/running Operationがなく、直近のterminal Operationからfailure delayが経過しているWorkspaceに対してcorrective Operationを作り、Outboxへ積みます。既定poll intervalは`10s`、failure delayは`1m`です。Fake Resource Controllerと結果Queue Consumerを組み合わせると結果はControl Planeへ反映されます。詳細は[Workspace Reconciler](reconciler.md)を参照してください。

ローカルAPIの最小起動例:

```sh
podman exec hako-dev-postgres pg_isready -U hako -d hako
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
make migrate
export HAKO_COGNITO_ISSUER='https://cognito-idp.<region>.amazonaws.com/<userPoolId>'
export HAKO_COGNITO_CLIENT_ID='<public-app-client-id>'
export HAKO_DEFAULT_WORKSPACE_IMAGE='hako/go:latest'
# Seed once in the local development database; this is configuration data, not a secret.
psql "$HAKO_DATABASE_URL" -c "INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp-local', 'aws', 'ap-northeast-1', '[\"microvm\"]') ON CONFLICT (id) DO NOTHING"
psql "$HAKO_DATABASE_URL" -c "INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ('rp-local', 'active', now()) ON CONFLICT (resource_plane_id) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at"
go run ./cmd/hako-api
```

CLI側は`HAKO_API_URL='http://127.0.0.1:8080'`を設定してから実行します。

DB更新には`internal/store/transaction.Within`を使います。OCC/serialization conflictではcallback全体が再実行されるため、callback内ではDB操作だけを行い、外部副作用はTransactional Outbox経由にします。Operation作成ではTenant内で一意のIdempotency-Keyを使い、同じkeyで異なる正規化済み要求が届いた場合はconflictとして返します。Workspace作成の原子性と並行要求の扱いは[Workspace作成フロー](workspace-create-flow.md)、Operation状態遷移とイベント履歴は[Operation状態遷移](operation-state-transitions.md)、冪等性規約は[ADR 0005](adr/0005-transaction-retry-and-idempotency.md)を参照してください。

実環境に対する変更では、対象AWS Accountの専用Roleとremote stateを設定したrootで次の順に実行し、planをレビューしてからapplyします。

```sh
terraform -chdir=infra/terraform/environments/dev/control-plane init
terraform -chdir=infra/terraform/environments/dev/control-plane validate
terraform -chdir=infra/terraform/environments/dev/control-plane plan
terraform -chdir=infra/terraform/environments/dev/control-plane apply
```

Resource Planeも同様に別root・別stateで実行します。state backendとAWS Roleが未設定の現時点では、実AWSへのplan/applyは行いません。

## リポジトリ構成

```text
cmd/                         実行バイナリ
internal/                    Hako内部パッケージ
docs/adr/                    設計判断記録
infra/terraform/modules/     再利用可能なTerraform module
infra/terraform/environments/ 環境・アカウントごとのTerraform root
```

Goは初期段階では単一moduleにまとめます。Control PlaneとResource Planeはプロセス境界とTerraform stateを分離し、共有するGo型やロジックは`internal`配下に置きます。
