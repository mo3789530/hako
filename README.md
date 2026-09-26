# Hako Architecture Design

## 1. 概要

Hakoは、**Lambda MicroVMを中心としたマルチテナント型Remote Development Platform**です。

ユーザーはローカルPCやブラウザからWorkspaceを作成し、VS Code、Shell、開発用DB、コンテナなどを安全に利用できます。

Hakoの基本思想は次の4層です。

```text
Control Plane
    = 管理・認証・状態管理

Resource Plane
    = 実際のAWSリソース操作

Data Plane
    = Developer ↔ Workspace通信

Workspace Runtime
    = Lambda MicroVM
```

Hakoでは以下を重要な設計原則とします。

```text
Workspace != MicroVM

Control Plane != Resource Plane

Control Plane != Data Plane

Hako User Identity != AWS IAM Identity
```

将来拡張として、GitHubを開発の入口、Hakoを実行・環境・自動化の中核とする **Hako Software Factory** を計画しています。GitHub App/Webhook、Repository Registry、Pipeline/Job、ephemeral self-hosted runner、Checks API、Agent Workloadを段階導入する構想です。これは未実装のロードマップであり、詳細な権限・信頼境界・実装順は[Hako Software Factory](docs/software-factory.md)を参照してください。

## 2. 全体アーキテクチャ

```text
                         Developer

                ┌──────────┴──────────┐
                │                     │
             Hako CLI              Browser
                │                     │
                └──────────┬──────────┘
                           │
                     HTTPS / OIDC
                           │
                           ▼
┌─────────────────────────────────────────────────┐
│             CONTROL PLANE ACCOUNT               │
│                                                 │
│ Cognito                                         │
│ API Gateway                                     │
│                                                 │
│ Hako API Lambda                                 │
│ Scheduler Lambda                               │
│ Reconciler Lambda                              │
│ Dispatcher Lambda                              │
│ Operation Result Consumer                      │
│ Cleanup Lambda                                 │
│                                                 │
│ Aurora DSQL                                    │
│                                                 │
│ Command / Result SQS                           │
└────────────────────────┬────────────────────────┘
                         │
                   Hako Command
                         │
                         ▼
┌─────────────────────────────────────────────────┐
│             RESOURCE PLANE ACCOUNT              │
│                                                 │
│ Resource Queue                                  │
│      │                                          │
│      ▼                                          │
│ Resource Controller Lambda                      │
│      │                                          │
│      ├── Lambda MicroVM                         │
│      ├── EFS                                    │
│      ├── Network Connector                      │
│      └── IAM                                    │
│                                                 │
│ Hako Gateway                                    │
└────────────────────────┬────────────────────────┘
                         │
                    HTTPS / WSS
                         │
                         ▼
               ┌──────────────────┐
               │ Lambda MicroVM   │
               │                  │
               │ hako-agent       │
               │ VS Code Server   │
               │ code-server      │
               │ Docker/containerd│
               │ App              │
               │ PostgreSQL       │
               │ Redis            │
               └──────────────────┘
```

## 3. AWSアカウント構成

Control PlaneとResource Planeは、**AWSアカウント単位で分離**します。

```text
AWS Organizations

├── Management Account
│
├── Hako Control Account
│
├── Resource Account Tokyo-01
│
├── Resource Account Tokyo-02
│
└── Resource Account US-01
```

Management Accountには通常のHako workloadを配置しません。

基本原則は、

```text
Management Account
    !=

Control Plane Account
    !=

Resource Plane Account
```

です。

将来的には、

```text
Customer AWS Account
      │
      └── Hako Resource Plane
```

というBYOCにも対応します。

## 4. Control Plane

Control PlaneはHako全体の **System of Record** です。

基本的にGo + AWS Lambdaで構成します。

```text
API Gateway
      │
      ▼
Hako API Lambda

Scheduler Lambda

Reconciler Lambda

Dispatcher Lambda

Cleanup Lambda
```

Control Planeの責務は、

```text
Authentication
Tenant
User
RBAC
Workspace
Desired State
Observed State
Placement
Quota
Billing
Session
Resource Mapping
Operation
```

です。

逆に、Control PlaneはAWS Resource Accountのリソースを直接操作しません。

## 5. Aurora DSQL

Control Planeの唯一の永続DBとして **Aurora DSQL** を利用します。

```text
Lambda
   │
   │ IAM Authentication
   ▼
Aurora DSQL
```

RDS Proxyは使用しません。

DB Passwordも基本的には保持しません。

IAM Roleを利用します。

```text
Hako API Lambda
      │
      │ dsql:DbConnect
      ▼
Aurora DSQL
```

Migration専用Roleだけ、

```text
dsql:DbConnectAdmin
```

を付与します。

## 6. DSQLの役割

DSQLには、

```text
tenants
users
tenant_members

workspaces
workspace_status

resource_planes
resource_plane_status

placements

resource_bindings

operations
operation_events

services
volumes

sessions

outbox_events
```

などを保存します。

Hakoの基本モデルは、

```text
Tenant
   │
   ▼
Workspace
   │
   ▼
Placement
   │
   ▼
Resource Plane
   │
   ▼
Resource Binding
   │
   ▼
Actual AWS Resource
```

です。

## 7. Workspace

Hakoで最も重要な概念です。

```text
Workspace != MicroVM
```

Workspaceは論理的・永続的なリソースです。

```text
Workspace

tenant
owner

runtime class
image

desired state
observed state

services
volumes
ports
sessions

placement
```

一方、MicroVMは一時的なComputeです。

```text
Workspace
   │
   ▼
MicroVM #1
   │
 terminate
   ▼
MicroVM #2
```

MicroVMが変わってもWorkspace IDは変わりません。

MVPではTenantあたり、削除完了前のWorkspaceを最大10件までとします。StoreはQuota slotをWorkspace/Operation/Outboxと同じDB transactionで確保し、同時作成でも上限を超えないようにします。Quota超過のHTTP応答はWorkspace作成APIの実装時に追加します。詳細は[Workspace上限と閲覧権限](docs/workspace-limits-and-visibility.md)を参照してください。

## 8. Workspace State

Desired StateとObserved Stateを分離します。

例えば、

```text
desired_state
    running

observed_state
    provisioning
```

です。

状態遷移は、

```text
pending
   ↓
provisioning
   ↓
running
   ↓
suspended
```

のようになります。

Reconcilerは、

```text
Desired State
      !=
Observed State
```

を検出してOperationを発行します。

## 9. Resource Plane

Resource Planeは、**Executor**として設計します。

Resource Planeには永続DBを持たせません。

```text
Resource Plane
│
├── Resource Controller
├── Gateway
├── IAM Role
│
└── AWS Resources
```

Resource Controllerは、

```text
Receive Operation
      ↓
AWS API
      ↓
Create / Update / Delete
      ↓
Report Result
```

を担当します。

Control PlaneのDSQLへ直接接続させません。

## 10. Resource Controller

Resource Controllerも基本的にはGo + Lambdaです。

```text
SQS
 │
 ▼
Resource Controller Lambda
 │
 ├── MicroVM API
 ├── EFS API
 └── Network API
```

AWS Credentialは保存しません。

```text
Resource Controller
      │
      ▼
IAM Role
      │
      ▼
AWS
```

です。

Control PlaneからCross Account AssumeRoleでAWSを操作する方式は基本採用しません。

## 11. Operation

Control PlaneからResource Planeへの操作はOperationとして扱います。

例:

```text
operation_id = op_123

workspace_id = ws_456

type =
ensure_running
```

Resource Planeは必ずidempotentにします。

```text
同じOperation
      ↓
2回受信
      ↓
同じ結果
```

となるようにします。

## 12. Aurora DSQLのOCC

Aurora DSQLはOptimistic Concurrency Controlを利用するため、Hako側もリトライ前提にします。

```text
Transaction
    │
 Conflict
    │
    ▼
Retry
```

特に、

```text
Workspace作成
+
Operation作成
```

などは1トランザクションで処理します。

## 13. Operation Queue

DB自体をQueueとして強く使わず、

```text
Aurora DSQL
     │
     ▼
Transactional Outbox
     │
     ▼
Dispatcher
     │
     ▼
SQS
```

にします。

これにより、

```text
DB commit成功
SQS send失敗
```

による不整合を防ぎます。

## 14. Resource Planeの登録情報

Resource Planeは複数存在できます。

```text
rp-tokyo-01

provider = aws
region   = ap-northeast-1

capabilities:

microvm
persistent-volume
container
private-network
```

Control Planeは、

```text
rp-tokyo-01
```

しか意識しません。

具体的な、

```text
AWS Account ID
VPC
Subnet
Security Group
EFS
IAM Role
```

などはResource Plane固有設定として扱います。

## 15. Placement

WorkspaceとMicroVMを直接結びつけません。

```text
Workspace
     │
     ▼
Placement
     │
     ▼
Resource Plane
```

とします。

例えば、

```text
ws_123
    ↓
rp_tokyo_01
```

です。

SchedulerがPlacementを決めます。

将来的には、

```text
Region
Capacity
Tenant policy
Cost
Enterprise isolation
```

などを考慮できます。

## 16. MicroVM

基本モデルは、

```text
1 Workspace
=
1 Lambda MicroVM
```

です。

ただしWorkspaceとMicroVM IDは別管理です。

Hako側では、

```text
small

standard

large
```

という抽象化されたRuntime Classを使います。

例:

```text
standard

Baseline
4 GB RAM
2 vCPU

Peak
16 GB RAM
8 vCPU

Disk
16 GB
```

## 17. MicroVM内部

標準Workspace Imageは、

```text
Lambda MicroVM

├── hako-agent
├── sshd
├── VS Code Server
├── code-server
├── Git
├── Go
├── Node
├── Docker/containerd
│
├── Application
├── PostgreSQL
└── Redis
```

などを含みます。

## 18. hako-agent

Go製の軽量Agentです。

```text
hako-agent

├── exec
├── shell
├── PTY
├── process management
├── SSH bridge
├── port forward
├── file transfer
├── container management
├── service management
└── heartbeat
```

できるだけstatic binaryにします。

MicroVMの外部入口は基本的にhako-agentへ集約します。

## 19. Container

MicroVM内部では、

```text
Docker / containerd
```

を利用可能にします。

既存プロジェクトは、

```bash
docker compose up
```

も利用できます。

Hako独自のService Catalogとも共存します。

## 20. `hako add`

ユーザーには、

```bash
hako add postgres
hako add redis
hako add mysql
hako add minio
```

というUXを提供します。

例えば、

```bash
hako add postgres
```

すると、

```text
Service Catalog
      ↓
postgres template
      ↓
hako-agent
      ↓
containerd
      ↓
PostgreSQL
```

となります。

## 21. Service Catalog

例えば、

```yaml
name: postgres

image: postgres:18

ports:
  - 5432

resources:
  memory: 1Gi
  cpu: 0.75

volume:
  path: /var/lib/postgresql/data

healthcheck:
  command:
    - pg_isready
```

のような内部定義を持ちます。

## 22. Storage

用途別にStorageを分離します。

```text
Source Code
    ↓
EFS

Development DB
    ↓
MicroVM Local Disk

Artifact / Backup
    ↓
S3

Persistent DB
    ↓
RDS / Aurora
```

基本的には、

```text
/workspace
    ↓
EFS
```

にします。

## 23. EFS

Workspace単位でEFS Access Pointを利用します。

```text
EFS

├── ws-001 Access Point
├── ws-002 Access Point
└── ws-003 Access Point
```

MicroVMが再作成されても、

```text
/workspace
```

は残ります。

## 24. Development DB

例えば、

```bash
hako add postgres
```

は、

```text
MicroVM Local Container
```

として動かします。

開発用なので高速性を優先します。

本当に永続DBが必要な場合は、

```bash
hako db create postgres --persistent
```

のような別機能として、

```text
RDS / Aurora
```

を利用します。

## 25. Authentication

ユーザー認証は、

```text
Amazon Cognito User Pool
```

です。

CLIでは、

```bash
hako login
```

↓

```text
Browser
   ↓
Cognito
   ↓
OAuth2 Authorization Code + PKCE
   ↓
Hako CLI
```

とします。

ユーザーへ、

```text
AWS Access Key

AWS Secret Access Key

IAM User
```

は配布しません。

## 26. Authorization

CognitoはIdentityを担当。

Hako DBがAuthorizationを担当します。

```text
Cognito sub
      │
      ▼
User
      │
      ▼
tenant_members
      │
      ▼
Tenant
      │
      ▼
Workspace
```

Cognito Groupは、

```text
hako-user
hako-admin
```

などプラットフォーム権限に限定します。

TenantそのものはDSQLで管理します。

## 27. Session

Workspaceアクセスには短期間の **Hako Session Ticket** を使います。

```text
Cognito JWT
      │
      ▼
Control Plane
      │
 Workspace RBAC
      │
      ▼
Hako Session Ticket
```

Ticketには例えば、

```text
user_id
tenant_id
workspace_id
resource_plane_id

permissions:
  vscode
  shell
  port-forward

expires_at
jti
```

を含めます。

## 28. Gateway

GatewayはLambdaにはしません。

長時間の、

```text
VS Code
SSH
Terminal
Port Forward
WebSocket
```

を扱うためです。

例えば、

```text
ECS/Fargate
```

などの常駐サービスにします。

GatewayはSSH Bastionではありません。

役割は、

```text
Session validation
Authorization
Routing
Stream proxy
Rate limiting
Audit
```

です。

## 29. Gatewayへの接続

GatewayにはSSHでは接続しません。

外部公開は基本、

```text
HTTPS / WSS
TCP 443
```

だけです。

```text
22
3000
5432
6379
8080
```

などは公開しません。

## 30. VS Code

ローカルVS CodeはRemote SSH互換を提供します。

```text
VS Code
   │
   │ SSH
   ▼
OpenSSH
   │
ProxyCommand
   ▼
hako ssh-proxy
   │
   │ WSS/TLS :443
   ▼
Hako Gateway
   │
   ▼
hako-agent
   │
   ▼
localhost:22
```

つまり、

```text
SSH
inside
WebSocket
inside
TLS
```

です。

インターネットにTCP/22は出ません。

## 31. Browser IDE

Browserからは、

```bash
hako open api-dev
```

で、

```text
Browser
   │
HTTPS/WSS
   ▼
Gateway
   │
   ▼
code-server
```

です。

code-serverはHako本体ではなく、**Hakoが管理するIDE Process**として扱います。

## 32. Port Forward

例えば、

```bash
hako forward api-dev 5432
```

すると、

```text
Local PC

127.0.0.1:5432
      │
      ▼
hako CLI
      │
      │ WSS
      ▼
Gateway
      │
      ▼
hako-agent
      │
      ▼
127.0.0.1:5432

PostgreSQL
```

となります。

DBポートは外部公開されません。

## 33. Data Plane

Control Planeを開発データの経路にしません。

```text
User
 │
 │ Session発行
 ▼
Control Plane

User
 │
 │ Data
 ▼
Resource Plane Gateway
 │
 ▼
MicroVM
```

です。

つまり、

```text
Control Plane
    =
Management

Gateway
    =
Data Plane
```

です。

## 34. PC切断時

ConnectionとWorkspace Lifecycleを分離します。

```text
CONNECTED
   ↓
network lost
   ↓
DISCONNECTED
   ↓
Grace Period
   ↓
IDLE
   ↓
Suspend
```

PC接続が切れても、

```text
MicroVM
containers
PostgreSQL
code-server
```

は即停止しません。

## 35. Terminal Session

TerminalもConnectionとは分離します。

```text
Terminal Session
     !=
Network Connection
```

例えば、

```text
PTY
 ↓
bash
 ↓
go test
```

をMicroVM側に保持できます。

MVPではtmux利用でも構いません。

## 36. Security

Hakoの基本セキュリティ原則です。

```text
No Public SSH

No Public Database Port

No Public Application Port

No AWS Credential on Client

Short-lived Session

Workspace-level Authorization

Tenant Isolation

Resource Account Isolation

IAM Role only

Audit Everything
```

## 37. AWS Resource Tag

作成するAWSリソースには、

```text
hako:managed-by=hako

hako:tenant-id=...

hako:workspace-id=...

hako:operation-id=...
```

などを付けます。

Control Plane DBとAWS actual stateがずれた場合も、タグから再発見できます。

## 38. Secret管理

DSQLには基本的にSecretを直接保存しません。

```text
DSQL
  │
  │ secret_ref
  ▼
Secrets Manager
```

とします。

AWS credentialはそもそも保存せずIAM Roleを使用します。

## 39. Development Environment

Hako自身の開発環境はDev Containerで統一します。

```text
Dev Container

├── Go
├── AWS CLI
├── Docker-in-Docker
├── PostgreSQL client
├── Redis client
├── protobuf
├── gopls
└── lint tools
```

ホストのDocker Socketは基本的にmountせず、

```text
Docker-in-Docker
```

を利用します。

## 40. Runtime Abstraction

Runtimeはinterface化します。

```go
type Runtime interface {
    EnsureRunning(...)
    Suspend(...)
    Resume(...)
    Delete(...)
}
```

最初は、

```text
Runtime

├── fake
└── aws-lambda-microvm
```

です。

将来的には、

```text
Firecracker
On-prem
Private Cloud
```

を追加できます。

## 41. Hako CLI

最終的なUXは、

```bash
hako login

hako create api-dev

hako code api-dev

hako open api-dev

hako shell api-dev

hako add postgres
hako add redis

hako services

hako forward api-dev 5432

hako suspend api-dev
hako resume api-dev

hako delete api-dev
```

程度まで単純化します。

## 42. Hakoの最終的な責務分離

```text
Cognito
    =
Who are you?


Aurora DSQL
    =
What should exist?


Scheduler
    =
Where should it run?


Resource Controller
    =
Make it exist.


AWS
    =
What actually exists?


Gateway
    =
How do you connect?


hako-agent
    =
What can you do inside the Workspace?


MicroVM
    =
Isolation boundary.
```

## 43. 最終アーキテクチャ

```text
                          Developer
                              │
                    ┌─────────┴─────────┐
                    │                   │
                  CLI                 Browser
                    │
                    ▼
                 Cognito
                    │
                    ▼

              CONTROL PLANE
┌────────────────────────────────────────────┐
│                                            │
│ API Gateway                                │
│                                            │
│ API Lambda                                 │
│ Scheduler                                  │
│ Reconciler                                 │
│ Dispatcher                                 │
│ Result Consumer                            │
│                                            │
│ Aurora DSQL                                │
│                                            │
│ Tenant                                     │
│ Workspace                                  │
│ Placement                                  │
│ Operation                                  │
│ Session                                    │
│                                            │
│ Command / Result SQS                       │
└───────────────────┬────────────────────────┘
                    │
              Command / Result
                    │
                    ▼

              RESOURCE PLANE
┌────────────────────────────────────────────┐
│                                            │
│ Resource Controller Lambda                 │
│                                            │
│             AWS APIs                       │
│                │                           │
│       ┌────────┼────────┐                  │
│       ▼        ▼        ▼                  │
│   MicroVM     EFS     Network              │
│       │                                    │
│       ▼                                    │
│   hako-agent                               │
│                                            │
│   Gateway ◄────────── Developer            │
│       │                                    │
│       │ WSS / HTTPS                        │
│       ▼                                    │
│   Workspace                                │
│                                            │
└────────────────────────────────────────────┘
```

## 44. Hakoのコアコンセプト

最も重要なのは、

```text
Tenant
   ↓
Workspace
   ↓
Placement
   ↓
Resource Plane
   ↓
Runtime Allocation
```

というモデルです。

そして、

```text
Control Plane
    =
System of Record

Resource Plane
    =
Executor

Gateway
    =
Zero Trust Workspace Proxy

MicroVM
    =
Isolation

Aurora DSQL
    =
Global Control State
```

という責務分離を維持します。

Hakoは最初は **Lambda MicroVMを使ったRemote Development Environment** として構築しますが、この構造であれば、将来的には **AI Agent Sandbox、CI Runner、BYOC、オンプレFirecracker、複数リージョンResource Plane** まで同じControl Planeで扱える基盤へ拡張できます。

## 45. 実装タスク案

以下は、この設計を実装可能な順序に分解したMVPタスクです。まずはAWSリソースを操作しない`fake` runtimeでControl PlaneからWorkspaceのライフサイクルまでを通し、その後にAWS統合を進めます。

### Phase 0: プロジェクト基盤

- [x] リポジトリ構成、Go module、共通設定・ログ・エラー処理を決める。
- [x] Dev Containerとローカル開発手順を用意する。
- [x] CIでformat、lint、unit testを実行する。
- [x] Terraformのディレクトリ構成、module分割、state管理、環境別設定方針を決める。
- [x] Terraformのformat・validateをCIに追加し、plan/applyの実行手順を文書化する。
- [x] Control PlaneとResource Planeを分離したTerraform root/moduleの雛形を用意する。
- [x] 設計上の決定事項（ADR）とAPI互換性方針を記録する。

### Phase 1: Control Planeのコアモデル

- [x] Tenant、User、Membership、Workspace/Status、Placement、Resource Plane/Status、Resource Binding、Operation、Session、Service、Volumeのデータモデルを定義する。
- [x] Aurora DSQL向けMigrationとIAM認証接続を実装し、同じMigrationを使うローカルPostgreSQL接続を用意する。
- [x] OCC競合時の再試行、トランザクション境界、冪等キー方針を実装する。
- [x] Workspace作成と`ensure_running` OperationをTransactional Outbox経由で同一トランザクションに記録する。
- [x] Operationイベントと状態遷移を永続化する。
- [x] DSQL schema、Workspace作成、Outbox、Operation処理のPostgreSQL統合テスト用fixtureとローカル実行方法を用意する。

### Phase 2: API、認証、認可

- [x] Cognito OAuth2 Authorization Code + PKCEによるCLIログインを実装する。
- [x] Hako APIにCognito access token認証ミドルウェアを実装する。
- [x] TerraformでControl Plane API Gateway HTTP APIとCognito JWT authorizerを定義する。
- [x] Cognito User Poolに`hako/api` custom scopeを定義し、CLI loginで要求する。
- [x] Terraformで`hako/api`を許可するpublic Cognito CLI app clientを作成する。
- [x] Go APIに認証不要の最小`GET /healthz`とaccess-token/`hako/api` scope必須の`GET /v1/health`を実装し、API Gatewayでも`/healthz`だけJWT認証から除外する。
- [x] CLIからkeyring内access tokenで保護APIを呼ぶ`hako api health`を追加する。
- [x] API handlerを標準`net/http`からEcho v5へ移行し、認証・scope middleware、route、JSONエラー応答、テストをEcho上で統一する。
- [x] API Gateway route/Lambda integrationを実装し、Gateway側でも`hako/api` scopeを強制する。Go Lambda adapterとpackage手順は[API Gateway Lambda](docs/api-gateway-lambda.md)を参照（Function/Role/DSQL接続のTerraformも実装済み。AWS applyとDB bootstrapは運用作業）。
- [x] 検証済みCognito `sub`を安定したHako Userへ対応付けるstore primitiveを実装する。
- [x] Tenant Membershipのrole lookup/require primitiveを実装し、非所属Tenantを拒否する。
- [x] Tenant membership routeで検証済みCognito `sub`をHako Userへ解決し、Tenant membershipを照合する。非所属・存在しないTenantの同一404、scope順序、DB障害応答をテストする。
- [x] `hako tenant membership <tenant-id>` CLIとAPI clientを追加し、Tenant roleを取得する。
- [x] Workspace storeでTenantごと最大10件を原子的に制限し、Workspaceが`deleted`になった時にQuotaを解放する。並行作成とslot再利用のintegration testを追加する。
- [x] `POST /v1/tenants/{tenant_id}/workspaces`と`hako create <tenant-id> <name>`を追加し、Quota超過を`workspace_quota_exceeded` HTTP 409としてCLIに表示する。
- [x] Workspace一覧・取得APIへRBACを適用する。Memberは自分のWorkspaceのみ、Tenant Owner/AdminはTenant内すべてとし、非所有・越境拒否を検証する。
- [x] Workspace一覧APIのlimit/offsetページネーション、`hako list`/`hako get` CLIを追加する。
- [x] Workspaceの停止・再開・削除API/CLIを追加し、Desired State・Operation・イベント・Outbox commandを原子的に記録する。非同期動作、冪等性、Quota解放条件は[Workspace lifecycle API](docs/workspace-lifecycle-api.md)を参照。
- [x] API入力検証、エラー形式、ページネーション、監査イベントを標準化する。JSON body制約、共通error envelope、offset paginationとactor-oriented mutation auditを実装し、監査の運用前提・未対応範囲を[Control Plane API Contract](docs/api-contract.md)に記載。

### Phase 3: 非同期制御とFake Runtime

- [x] Outbox DispatcherからResource Plane別SQSへOperation commandをat-least-once配送し、opt-inのEventBridge Scheduled Lambda、最小IAM、失敗alarmを追加する。Lease回収、指数backoff、AWS設定・有効化順序は[Outbox Dispatcher](docs/outbox-dispatcher.md)を参照。
- [x] Schedulerでactive状態・capability・任意region制約を確認してPlacementを選択し、既存Workspace数でtie-breakする。[Scheduler Placement](docs/scheduler-placement.md)。
- [ ] Tenant別Placement policy、容量reservation、health/cost/isolation条件をSchedulerに追加する（Tenant policy、Resource Plane別Workspace/CPU/RAM予約とRuntime Class別需要設定、Health優先・unhealthy除外・5分freshness TTL、TenantのCost ceiling/Isolation floorを実装済み。Health report source/historyの保存基盤を追加。Resource Plane reporter、AWS probe、安全なingress/queue、scheduleは未実装）。[Tenant Placement Policy](docs/tenant-placement-policy.md)、[Resource Plane Capacity](docs/resource-plane-capacity.md)、[Resource Plane Health](docs/resource-plane-health.md)。
- [x] ReconcilerでDesired/Observed State差分からcorrective Operation、event、Outbox commandを原子的に生成する。競合claim、state mapping、failure cooldownは[Workspace Reconciler](docs/reconciler.md)を参照。
- [x] Operationを重複受信しても安全なFake Resource ControllerとFake Runtimeを実装する。[Fake Resource Controller](docs/fake-resource-controller.md)。
- [ ] リトライ、可視性タイムアウト、DLQ、Operation timeout、Cleanupを実装する（Worker retry/backoff・visibility延長・Runtime実行timeout・Control Plane stale Operation回収、queue/DLQ監視alarm、schema v2 Workspace revisionによるControl Plane/Fake Runtime fencing、既定30分を超えた未実行commandの拒否を実装済み。実Runtimeのdurable fencing、部分成功の補償Cleanup、AWS適用・DLQ確認/redriveは未完了）。[Fake Resource Controller](docs/fake-resource-controller.md)、[Resource Plane command/result protocol](docs/resource-plane-command-protocol.md)、[Resource Plane queues and DLQ](docs/resource-plane-queues-and-dlq.md)、[Workspace Reconciler](docs/reconciler.md)。
- [x] APIからWorkspace作成、起動、停止、削除までをFake Runtimeで通す。結果Queue ConsumerがOperationとObserved Stateを反映する（PostgreSQL integration test、in-memory queue）。[Operation Result Consumer](docs/operation-results.md)。
- [x] Fake Runtimeを使い、ログイン後のWorkspace作成から起動・状態確認・停止・削除までを通すintegration E2Eを追加する。[Authenticated CLI Lifecycle E2E](docs/cli-lifecycle-e2e.md)。
- [x] E2Eで重複Operation、結果ack失敗後の再試行、非同期状態遷移、他ユーザーWorkspaceへの認可拒否を検証する。[Authenticated CLI Lifecycle E2E](docs/cli-lifecycle-e2e.md)。

### Phase 4: Resource Plane AWS統合

- [x] Resource Plane登録・構成形式と必要Capabilityを定義する（version付きJSON manifestとstrict validatorを用意し、Dispatcher/Result Consumerから複数QueueをRegion-awareに利用）。DSQLへの登録自動化は未実装。[Resource Plane registration manifest](docs/resource-plane-registration.md)。
- [ ] TerraformでResource Controller Lambda、Queue、IAM Role、結果通知経路を構築する（command/result Queue・DLQ・redrive policy・queue policy・最小IAM Role・Log Groupに加え、明示opt-inのFake Runtime専用LambdaとSQS event source mappingを定義。実Runtime・本番Lambda・AWS applyは未完了）。[Resource Plane queues and DLQ](docs/resource-plane-queues-and-dlq.md)、[Resource Controller Lambda](docs/resource-controller-lambda.md)。
- [ ] TerraformでControl Plane側のAPI Gateway、Lambda、Cognito、DSQL、Outbox配信基盤を構築する（HTTP API、API Lambda、DSQL、最小DB/IAMとopt-inのEventBridge Scheduled Outbox Dispatcher Lambdaを実装。Cognito User Pool、AWS apply、SQL role bootstrap・migrationの実環境適用は未完了。Dispatcherに専用Queueは作らず、各Resource Planeのcommand queueへ直接配信する）。[Control Plane Terraform](docs/control-plane-terraform.md)。
- [ ] Hako API LambdaをZIP/custom runtimeからDocker/OCIコンテナイメージ方式へ移行する（Lambda Web Adapter、arm64 multi-stage build、同一Regionのimmutable ECR・scan/lifecycle、digest固定の別Lambda候補、API Gateway切替フラグ、ローカルHTTP smoke testを実装・検証済み。AWS candidateへのAPI Gateway v2 event直接invoke、ECR pushと実環境cutover/rollback検証は未完了）。[API Gateway and Lambda](docs/api-gateway-lambda.md)。
- [ ] TerraformでResource Plane側のGateway、ネットワーク、ログ・監視基盤を構築する（QueueのDLQ/滞留/backlog監視とFake Controller errors alarm、保持期間を実装。Gateway、VPC/subnet、Flow Logs、production監視は未実装）。[Terraform module documentation](docs/terraform-modules.md)。
- [x] 現在のTerraform moduleの入力・出力、必須タグ、IAM最小権限、dev環境差分を文書化する（Control Plane/Resource Planeの現行2 moduleを網羅。新module追加時は本書も更新）。[Terraform module documentation](docs/terraform-modules.md)。
- [ ] Lambda MicroVMのEnsureRunning、Suspend、Resume、Delete実装を追加する。
- [ ] 作成するAWSリソースにtenant/workspace/operationタグを付与し、タグによる再発見を実装する（tag schemaとscope helper/test、Control/Resource shared infraの所有tagを実装。Workspace AWS resource tagging/discoveryはRuntime未実装）。[AWS resource ownership tags](docs/aws-resource-tags.md)。
- [ ] リソース作成失敗・部分成功時の補償処理とCleanupを実装する。
- [x] Control PlaneとResource Plane間の認可・version付きcommand/result envelopeを定義する（AWS IAM principal、SQS resource policy、schema v2のWorkspace revision fencingを追加。Control Planeのstale-result拒否とFake Runtimeのprocess-local fenceまで実装。payload HMAC、BYOC等の非AWS transport署名と実Runtimeのdurable fenceは未実装）。[Resource Plane command protocol](docs/resource-plane-command-protocol.md)。
- [ ] AWS検証環境でWorkspace作成からGateway接続、削除までを確認するE2Eテストを追加する（Gateway/実Runtimeが未実装のため未実行）。[AWS E2E environment and cleanup](docs/aws-e2e.md)。
- [x] AWS E2Eテストの実行条件、専用アカウント・環境、作成リソースのcleanup手順を定義する（本番/Management Accountを禁止し、account identity確認と手動レビューを要求。実行コード・cleanup自動化は別タスク）。[AWS E2E environment and cleanup](docs/aws-e2e.md)。

### Phase 5: Workspace永続化とAgent

- [ ] WorkspaceごとのEFS Access Point作成・削除と`/workspace` mountを実装する。
- [ ] 標準MicroVM Imageとhako-agentの配布・起動方式を定義する。
- [ ] hako-agentにheartbeat、exec、shell/PTY、プロセス管理を実装する。
- [ ] MicroVM再作成後にWorkspace IDとソースデータが維持されることを確認する。
- [ ] Secret参照をSecrets Managerに接続し、平文Secretをログ・DBに残さない。

### Phase 6: Gatewayと開発者接続

- [ ] 短命Hako Session Ticketの発行・署名・失効・権限検証を実装する。
- [ ] GatewayをECS/Fargate上に配置し、HTTPS/WSSでSession検証・ルーティングする。
- [ ] CLIの`hako ssh-proxy`とVS Code Remote SSH接続を実装する。
- [ ] CLIのshell、port forward、file transferを実装する。
- [ ] 切断後もPTY/processを維持し、Grace Period後にSuspendするLifecycleを実装する。
- [ ] レート制限、接続上限、監査記録、Gatewayヘルス監視を実装する。

### Phase 7: Service Catalogと利用者向けCLI

- [ ] Service CatalogのSchemaとPostgreSQL/Redisなどの初期Templateを定義する。
- [ ] hako-agent経由でServiceの起動・停止・状態確認を実装する。
- [ ] `hako add`、`hako services`を実装する。
- [ ] `hako create/code/open/shell/forward/suspend/resume/delete`のCLI UXを仕上げる。
- [ ] Resource Class、Quota、操作確認、利用状況表示を追加する。

### Phase 8: セキュリティ・運用・拡張

- [ ] TerraformでControl Planeと各Resource Planeを別AWS Accountに分けた環境を整備する。
- [ ] 最小権限IAM、Cognito設定、暗号化、監査ログ、メトリクス、アラームを整備する。
- [ ] Tenant越境アクセス、期限切れTicket、リプレイ、重複Operation、障害復旧を検証する。
- [ ] E2EテストをCIに組み込み、Fake Runtimeの必須実行とAWS環境の定期実行を設定する（Fake Runtime/PostgreSQL integration testはPR・main CIで毎回実行。AWS実環境の定期実行と専用OIDC role/environmentは未設定）。
- [ ] E2E失敗時にログ、Operation履歴、関連AWS resource IDから原因を追跡できるようにする。
- [ ] Backup/復旧、Workspace cleanup、孤立AWSリソース検出の運用手順を整備する。
- [ ] BYOC、複数リージョン、Persistent DB、Billingの要件を個別ADRと後続タスクに分ける。

### Phase 9: Hako Software Factory

以下はWorkspace Runtime/Gatewayの基盤整備後に進める追加機能です。GitHub Appの権限・Webhookの検証を先行させ、ユーザー提供のWorkflow/Issue/PRデータはすべて信頼されない入力として扱います。詳細な設計と実装順は[Hako Software Factory](docs/software-factory.md)に記載します。

- [ ] GitHub Appを登録し、Secrets Manager保管、最小権限、Installation tokenの短期利用・失効方針を実装する。
- [ ] TenantとGitHub App Installation/Repositoryを対応付け、Repository Registry APIとRBACを追加する。
- [ ] Webhook ingressでraw bodyのHMAC-SHA256検証、Delivery ID冪等化、サイズ制限、監査、再送を実装する（raw bodyのHMAC検証、UUID形式Delivery ID、初期event/action allowlist、body上限のライブラリとテストを追加。HTTP route、durable inbox/idempotency、Secrets Manager、監査・再送は未実装）。[Hako Software Factory](docs/software-factory.md)。
- [ ] GitHub webhookをversion付きHako Repository Eventへ正規化し、durable inbox/outbox経由で処理する。
- [ ] Job/Agent/Preview Runの共通状態・Operation・timeout/cancel/log/artifact metadataを設計する。Workspaceとのlifecycle分離を維持する。
- [ ] Fake Runtime上でRepository commitに対する`go test ./...` Jobを実行し、結果とredacted logsを保存する。
- [ ] GitHub Checks APIでqueued/in-progress/completed check、commit SHA照合、summary/annotationを返す。
- [ ] GitHub Actions `workflow_job.queued`を受け、専用権限でephemeral runnerを登録し、1 Job実行後にderegister・Runtime cleanupする。
- [ ] `.hako/factory.yaml`のversioned strict schema、allowlisted step、network/secret policyを実装し、既存GitHub Actionsと共存させる。
- [ ] IssueからAgent Runを開始し、明示的な最小write permissionでbranch/PRを作成するworkflowを追加する。
- [ ] PR Preview Workspace、artifact/SBOM storage、Software Catalog、OIDC deploy policyを分離タスクとして実装する。
- [ ] GitHub App install→PR→Webhook→isolated `go test`→GitHub Check→logs保管→ephemeral Runtime破棄のSoftware Factory E2Eを追加する。

### MVP完了条件

- [ ] ユーザーがログインし、Tenant内にWorkspaceを作成できる。
- [ ] Workspace作成からOperation配信、Resource Controller実行、状態反映まで追跡できる。
- [ ] Fake Runtimeで起動・停止・削除と再試行を確認できる。
- [x] Fake Runtimeを使う主要WorkspaceライフサイクルE2EがCIで実行される（PostgreSQL integration suiteをPR・main CIで実行）。
- [ ] TerraformでControl PlaneとResource Planeの検証環境を再現できる。
- [ ] AWS RuntimeでMicroVMを起動し、EFS上のWorkspaceデータを保持できる。
- [ ] VS CodeまたはCLI shellからGateway経由で接続でき、公開SSH/DBポートを必要としない。
- [ ] AWS統合E2EでWorkspace作成、接続、停止、削除を検証できる。
- [ ] Tenant/Workspace単位の認可と監査が全操作に適用される。

Software Factoryの将来MVPは、GitHub App installからPR Check表示までを1 repository/1 test jobで安全に通すことです。現行MVPとは別の後続マイルストーンです（[設計・前提](docs/software-factory.md)）。
