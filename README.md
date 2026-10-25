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

基本モデルは、Workspaceごとに同時に最大1つのMicroVM Allocationを持つ形です。
MicroVMは交換可能な実行資源であり、Workspaceと同じ寿命ではありません。

```text
1 Workspace
→ 0..1 current Lambda MicroVM Allocation
```

です。

ただしWorkspaceとMicroVM IDは別管理です。
Lambda MicroVMは最大8時間で終了する可能性があり、suspendは同じVMの
memory/local diskを保つだけです。終了後の自動置換にはEFS永続化とagent復旧が必要で、
Task 44/45完了までは有効化しません。詳細は[Workspace Runtime契約](docs/workspace-lambda-microvm.md)。

`small`/`standard`/`large`のCPU・memory値は現時点では目標案であり、Lambda
MicroVM APIがそのサイズ指定を提供することは確認できていません。Runtime adapterは
実際に提供できるprofileだけを許可し、未対応classをfail closedにします。

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

この値はMicroVM Imageのbaseline sizingです（Run APIでMicroVMごとにCPU/RAMを指定する設計ではありません）。AWSの公開サイズ表では4 GiB baseline / 2 vCPUに対して最大16 GiB / 8 vCPUへburstし、disk上限は16 GiBです。1 MicroVMの存続時間は最大8時間、ARM64のみです。Quotaとnested containerの実動作はTask 61のPoC待ちで、Runtime Class名だけでは隔離・capacityを保証しません。[Lambda MicroVM runtime constraints](docs/lambda-microvm-workloads.md)

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

最終的なUXは名前だけでWorkspaceを解決する形を目指します。現時点のCLIではTenant IDとWorkspace IDを明示します。

```bash
hako add <tenant-id> <workspace-id> postgres [instance-name]
hako add <tenant-id> <workspace-id> redis [instance-name]
hako services <tenant-id> <workspace-id>
hako service usage <tenant-id> <workspace-id>
hako service rotate-credentials <tenant-id> <workspace-id> postgres
```

現時点の`hako add`は認証・認可済みAPIを通じてdesired-state metadataを登録し、revision付きOutbox commandをWorkspace PlacementのResource Plane queueへ配送します。Resource Plane consumerはGuestを呼ぶ直前にHMACでControl Planeから最新Service revision/stateを取得するため、古いSQS recordは実行せず最新状態へreconcileします。その後、durable journal上のLambda MicroVM bindingを再検証し、port 8080限定の短命AWS tokenとone-use CapabilityでGuestを呼び、HMAC状態報告を返します。Terraformにはdefault-offのqueue/event-source/IAM wiringと監視alarmを追加しましたが、mock plan検証のみでAWSには未適用です。Workspace init imageには`nerdctl-full`と、別途version/SHA-256固定するARM64 `slirp4netns`を含める構成、root initがrootless containerdを起動・監督する`HAKO_WORKSPACE_SERVICES_ENABLED=true` opt-inを追加しました（既定off）。PostgreSQL資格情報は`hako service rotate-credentials`でgeneration付きに更新し、Guest側適用とhealth確認後にSecrets Managerの`AWSCURRENT`を進めます。ARM64イメージの実ビルドとLambda MicroVM内でのコンテナ起動、ネットワーク・resource limit、AWS接続は未検証です。Service削除は旧MicroVM世代を含むcleanup確認までtombstoneを保持しますが、実環境での動作確認は未完了です。登録直後の状態は`observed_state=pending`であり、実環境のhealthcheck完了を意味しません。詳細は[Workspace Agent shell and PTY](docs/workspace-agent-shell.md)、[Service credentials](docs/workspace-service-secrets.md)、[Task 84](tasks/task-84-workspace-service-runtime.md)を参照してください。

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

です。初期CatalogはPostgreSQLとRedisに限定し、公式ARM64 manifest digestでimageを固定し、resource limits、private-only ports、volume/healthcheckを固定します。digest更新時はCatalog変更とレビューが必要です。MySQL/MinIOは未提供です。API/CLIの挙動と現在の制約は[Workspace CLI](docs/workspace-cli.md)を参照してください。

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

これは最終UXの名前指定例です。現行MVPはTenant/Workspace IDを指定し、
`hako forward <tenant-id> <workspace-id> <remote-port> [local-port]`で
`127.0.0.1`だけに待受けます。CLIから任意の宛先ホストは指定できません。
詳細は[Workspace CLI](docs/workspace-cli.md)を参照してください。

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

実装の確認結果と未完了の受入条件は[Security review](docs/security-review.md)を参照してください。既知Go脆弱性の修正とCI検査を追加しています。実AWSの隔離・権限・運用確認は後続タスクです。

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

目標とする最終UXは、

```bash
hako login

hako create api-dev
hako wait api-dev --state running

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

現行MVPの`hako shell`/`hako forward`はWorkspace名ではなくTenant IDと
Workspace IDを受け取ります。実装済み範囲と接続方法は
[Workspace CLI](docs/workspace-cli.md)を参照してください。

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

未完了項目を依存関係・受入条件付きで実装担当へ渡す場合は、[Task Board](tasks/README.md)と個別task specを参照してください。READMEのチェックリストは全体ロードマップ、Task Boardは次の実装単位と着手順を示します。

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
- [x] Tenant membershipで保護したusage APIと`hako usage <tenant-id>`を追加する。Workspace 10枠、concurrent Workload 5枠、Tenant-configurable Preview concurrency/lifetimeを表示し、terminal確定まで予約を保持する。値は設定quotaで、実測resource usageや請求額ではない。
- [x] Workspace Member visibility付きService usage APIと`hako service usage <tenant-id> <workspace-id>`を追加し、Serviceのcatalog-request reservationとadmission limitを表示する。値は実測MicroVM使用量ではない。
- [x] Tenant Owner/Admin向けQuota read/update APIと`hako tenant quota get|set`を追加。Workspace 10、Workload 5、Preview 5のplatform ceiling以下で設定し、現在usageを下回る変更を拒否する。admissionとのOCC直列化は[Tenant quotas](docs/tenant-quotas.md)を参照。
- [x] Workspaceの停止・再開・削除API/CLIを追加し、Desired State・Operation・イベント・Outbox commandを原子的に記録する。非同期動作、冪等性、Quota解放条件は[Workspace lifecycle API](docs/workspace-lifecycle-api.md)を参照。
- [x] API入力検証、エラー形式、ページネーション、監査イベントを標準化する。JSON body制約、共通error envelope、offset paginationとactor-oriented mutation auditを実装し、監査の運用前提・未対応範囲を[Control Plane API Contract](docs/api-contract.md)に記載。

### Phase 3: 非同期制御とFake Runtime

- [x] Outbox DispatcherからResource Plane別SQSへOperation commandをat-least-once配送し、opt-inのEventBridge Scheduled Lambda、最小IAM、失敗alarmを追加する。Lease回収、指数backoff、AWS設定・有効化順序は[Outbox Dispatcher](docs/outbox-dispatcher.md)を参照。
- [x] Schedulerでactive状態・capability・任意region制約を確認してPlacementを選択し、既存Workspace数でtie-breakする。[Scheduler Placement](docs/scheduler-placement.md)。
- [ ] Tenant別Placement policy、容量reservation、health/cost/isolation条件をSchedulerに追加する（Tenant policy、Resource Plane別Workspace/CPU/RAM予約とRuntime Class別需要設定、Health優先・unhealthy除外・5分freshness TTLを実装済み。Task 48でHMAC report API/client、replay protection、queue・固定Lambda MicroVM image version・VPC egress connector・Controller function・EFS metadata probe、EMF metrics、default-off exact-ARN IAM/route/alarmsを実装し、unit/PostgreSQL統合テスト済み。metadata probeは実起動・network isolation・実EFS mountを証明しない。paired Secret provisioning/rotation、実AWS配備・検証は未完了）。[Tenant Placement Policy](docs/tenant-placement-policy.md)、[Resource Plane Capacity](docs/resource-plane-capacity.md)、[Resource Plane Health](docs/resource-plane-health.md)。
- [x] ReconcilerでDesired/Observed State差分からcorrective Operation、event、Outbox commandを原子的に生成する。競合claim、state mapping、failure cooldownは[Workspace Reconciler](docs/reconciler.md)を参照。
- [x] Operationを重複受信しても安全なFake Resource ControllerとFake Runtimeを実装する。[Fake Resource Controller](docs/fake-resource-controller.md)。
- [ ] リトライ、可視性タイムアウト、DLQ、Operation timeout、Cleanupを実装する（Worker retry/backoff・visibility延長・Runtime実行timeout・Control Plane stale Operation回収、queue/DLQ監視alarm、schema v2 Workspace revisionによるControl Plane/Fake Runtime fencing、既定30分を超えた未実行commandの拒否を実装済み。実Runtimeのdurable fencing、部分成功の補償Cleanup、AWS適用・DLQ確認/redriveは未完了）。[Fake Resource Controller](docs/fake-resource-controller.md)、[Resource Plane command/result protocol](docs/resource-plane-command-protocol.md)、[Resource Plane queues and DLQ](docs/resource-plane-queues-and-dlq.md)、[Workspace Reconciler](docs/reconciler.md)。
- [x] APIからWorkspace作成、起動、停止、削除までをFake Runtimeで通す。結果Queue ConsumerがOperationとObserved Stateを反映する（PostgreSQL integration test、in-memory queue）。[Operation Result Consumer](docs/operation-results.md)。
- [x] Fake Runtimeを使い、ログイン後のWorkspace作成から起動・状態確認・停止・削除までを通すintegration E2Eを追加する。[Authenticated CLI Lifecycle E2E](docs/cli-lifecycle-e2e.md)。
- [x] E2Eで重複Operation、結果ack失敗後の再試行、非同期状態遷移、他ユーザーWorkspaceへの認可拒否を検証する。[Authenticated CLI Lifecycle E2E](docs/cli-lifecycle-e2e.md)。

### Phase 4: Resource Plane AWS統合

- [x] Resource Plane登録・構成形式と必要Capabilityを定義する（version付きJSON manifestとstrict validatorを用意し、Dispatcher/Result Consumerから複数QueueをRegion-awareに利用）。DSQLへの登録自動化は未実装。[Resource Plane registration manifest](docs/resource-plane-registration.md)。
- [ ] TerraformでResource Controller Lambda、Queue、IAM Role、結果通知経路を構築する（command/result Queue・DLQ・redrive policy・queue policy・最小IAM Role・Log Groupに加え、Fake RuntimeとLambda MicroVM RuntimeのARM64 Lambda/event mappingを明示opt-inで定義。MicroVM側は固定image・journal-prefix IAMとdynamic EFS role prerequisiteを検証し、Fakeとのqueue共有を拒否する。dev rootでは未設定。実image/agent/network/lifecycle PoC、AWS IAM enforcement、AWS applyは未完了）。[Resource Plane queues and DLQ](docs/resource-plane-queues-and-dlq.md)、[Resource Controller Lambda](docs/resource-controller-lambda.md)、[Terraform modules](docs/terraform-modules.md)。
- [ ] TerraformでControl Plane側のAPI Gateway、Lambda、Cognito、DSQL、Outbox配信基盤を構築する（HTTP API、API Lambda、DSQL、最小DB/IAMとopt-inのEventBridge Scheduled Outbox Dispatcher Lambdaを実装。Workload scheduler queue/DLQはDispatcher opt-in時に作成。Cognito User Pool、Workload consumer/worker、AWS apply、SQL role bootstrap・migrationの実環境適用は未完了）。[Control Plane Terraform](docs/control-plane-terraform.md)。
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
- [ ] 標準MicroVM Imageとhako-agentの配布・起動方式を定義する（protocol/backendと`cmd/hako-agent`のport 8080 listener、image-owned public-key loaderを実装。MicroVM image build/run-hook wiring、production key distribution/rotation、AWS image/network PoCは未完了）。[Lambda MicroVM Workload Controller](docs/lambda-microvm-workloads.md)。
- [ ] hako-agentにheartbeat、exec、shell/PTY、プロセス管理を実装する（Workspace向けLinux PTYとGateway WSS、専用Session権限付き`hako exec`、versioned Guest heartbeat/30秒lease、one-use Guest Ticket、出力量/実行時間制限を実装。PTYは同一MicroVM上で切断中も維持し、同一ユーザーの再接続と1 MiB出力保持をlocal test済み。実MicroVM image・`/dev/pts`検証、production key provisioningは未完了）。[Workspace Agent shell and PTY](docs/workspace-agent-shell.md)。
- [ ] MicroVM再作成後にWorkspace IDとソースデータが維持されることを確認する。
- [ ] Secret参照をSecrets Managerに接続し、平文Secretをログ・DBに残さない。

### Phase 6: Gatewayと開発者接続

- [ ] 短命Hako Session Ticketの発行・署名・失効・権限検証を実装する（Ed25519の5分ticketとRBAC確認付き発行/失効API、HTTPS/HMAC active-session API/clientとPostgreSQL照合、WSS-to-PTY/localhost port-forward local E2Eは実装。production key provisioning/rotation、Gateway client配備、production revocation確認は未完了）。[Workspace Session Ticket](docs/workspace-session-ticket.md)。
- [ ] Gatewayを常駐サービスとして配置し、HTTPS/WSSでSession検証・ルーティングする（TLS-only WSS core、共有接続上限、同一Session TicketのGateway複数台間同時利用拒否、Workspace単位30新規Lease/分の共有rate limit、実PTY接続とallowlisted port-forwardをlocal E2E済み。Resource Planeのdefault-off HTTPS ALB/private Fargate deployment等をTerraform化してmock plan検証済み。構造化ログと低カーディナリティEMFも実装。AWS配備、production keys/secrets/certificate/DNS/network、実MicroVM接続とCloudWatch ingestionは未完了）。[Workspace HTTPS/WSS Gateway](docs/workspace-gateway.md)。
- [ ] VS Code Remote SSH接続を実装する（`hako ssh-proxy`のlocal TLS/WSS ProxyCommandは実装。MicroVM sshd/router deploymentは未検証）。
- [ ] CLIのshell、browser IDE、SSH proxy、port forward、file transferを実装する（`hako shell`はLinux terminal resize/framingに加え、TLS/WSS切断後に新Sessionで同じGuest PTYへ再接続するlocal E2Eを実装。PTYはMicroVM置換後には維持されない。`hako open`のloopback-only browser tunnel、per-stream VS Code Session、fixed Guest bridge、Lambda adapter、EFS UID/GIDで動作するpinned/checksummed code-server process lifecycleをlocal実装。ARM64 Workspace image buildと実MicroVM接続は未検証。`hako wait`、`hako ssh-proxy`、`hako cp`、`hako forward`もlocal実装。`hako cp`は独立したsigned Agent file-transfer routeで、SSH/SFTP host-keyに依存しない。Production Gateway/MicroVM routingは未検証）。[Workspace CLI](docs/workspace-cli.md)
- [ ] 切断後もPTY/processを維持し、Grace Period後にSuspendするLifecycleを実装する（同一MicroVM内のPTY detach/reattach、durable lease、allocation-bound Gateway heartbeat、Owner/Admin向けdefault-off policy API/CLI、fenced Suspend Operation/Outbox/system-audit sweep、EventBridge Lambda entrypoint、default-off Terraform schedule/IAM contractはlocal実装・PostgreSQL/Terraform検証済み。Gateway WSS→署名lease API→PostgreSQLのdisconnect/reconnect/sweep後、Outbox Dispatcher→Resource Plane queue→Resource Controller fake→結果反映までPostgreSQLで検証。`hako resume --wait`でrunning状態まで待機可能。実MicroVM Guest reconnectとAWS配備は[Task 85](tasks/task-85-workspace-idle-suspend.md)で追跡）。[Workspace connection lifecycle](docs/workspace-connection-lifecycle.md)。
- [ ] レート制限、接続上限、監査記録、Gatewayヘルス監視を実装する。

### Phase 7: Service Catalogと利用者向けCLI

- [x] Versioned Service Catalog schemaとPostgreSQL/Redis初期Templateを追加（公式ARM64 manifest digest固定image、resource limits、private ports、volume/healthcheck policy）。
- [ ] hako-agent経由でServiceの起動・停止・状態確認を実装する（revision付きOutbox/Resource Plane queue route、固定Catalog Guest reconcile handler/rootless `nerdctl` adapter、root initによるAgent child再起動耐性revision/replay fence、HMAC状態報告API、SQS Lambda consumer、Terraform default-off配線とPostgreSQL用のSecrets Manager生成credential providerを実装・fake/local検証。SecretはResource Planeから正確なMicroVMへHTTPSで渡し、Guest内の一時mode-0600 env-file経由でのみnerdctlへ注入する。`hako service rotate-credentials`はgeneration付きAWSPENDINGをDBへ適用しhealth確認後にAWSCURRENTへpromoteする。MicroVM image daemon/実PoCとMicroVM世代をまたぐdurable cleanup確認は未完了。詳細は[Workspace Service generated credentials](docs/workspace-service-secrets.md)。
- [x] 認証済みWorkspace-scoped APIと`hako add`、`hako services`、desired-stateの`hako service start|stop|remove`を追加。Task 84でResource Plane queue、固定TemplateのGuest reconcile、revision/binding-fenced health report、rootless `nerdctl` executor、生成credential lifecycleとgeneration-fenced cleanupまでlocal実装・fake/PostgreSQL検証済み。Terraformはdefault-offで未適用。実Lambda MicroVM上のcontainer/network/resource-limit動作は未検証。
- [ ] `hako create/code/open/shell/forward/suspend/resume/delete`のCLI UXを仕上げる。
- [ ] Resource Class、Quota、操作確認、利用状況表示を追加する（Workspace 10枠/Workload concurrent 5枠、Preview concurrency/lifetime、Tenant設定可能なArtifact quota（10 GiB default/100 GiB max、最大100 MiB/Run予約）、Workspace Service per-Workspace memory/CPU caps（2 GiB/1500mCPU default/hard ceiling、Tenantは下げられる）はOwner/Admin API/CLIで設定しTenant OCC下でadmission。`hako usage`/`hako service usage`はreserved usageを表示し、`hako tenant quota set`は現在usage/limitの表示と明示確認に対応。Artifact usageは登録metadataで、live S3 inventory、実測resource usage、他の破壊的/高影響操作確認は未完了）。

### Phase 8: セキュリティ・運用・拡張

- [ ] TerraformでControl Planeと各Resource Planeを別AWS Accountに分けた環境を整備する。
- [ ] 最小権限IAM、Cognito設定、暗号化、監査ログ、メトリクス、アラームを整備する。
- [ ] Tenant越境アクセス、期限切れTicket、リプレイ、重複Operation、障害復旧を検証する。
- [ ] E2EテストをCIに組み込み、Fake Runtimeの必須実行とAWS環境の定期実行を設定する（Fake Runtime/PostgreSQL integration testはPR・main CIで毎回実行。AWS実環境の定期実行と専用OIDC role/environmentは未設定）。
- [ ] E2E失敗時にログ、Operation履歴、関連AWS resource IDから原因を追跡できるようにする。
- [ ] Backup/復旧、Workspace cleanup、孤立AWSリソース検出の運用手順を整備する。
- [x] BYOC ([ADR 0010](docs/adr/0010-customer-owned-resource-planes.md))、複数リージョン ([ADR 0011](docs/adr/0011-multi-region-resource-planes.md))、Persistent DB ([ADR 0012](docs/adr/0012-persistent-development-databases.md))、Billing ([ADR 0013](docs/adr/0013-usage-metering-and-billing.md)) の要件を個別ADRに記録する（要件整理のみ。実装は未承認・未着手）。

### Phase 9: Hako Software Factory

以下はWorkspace Runtime/Gatewayの基盤整備後に進める追加機能です。GitHub Appの権限・Webhookの検証を先行させ、ユーザー提供のWorkflow/Issue/PRデータはすべて信頼されない入力として扱います。詳細な設計と実装順は[Hako Software Factory](docs/software-factory.md)に記載します。

- [ ] Tenant専用GitHub Appを追加し、private key/OAuth/Webhook Secret、承認reader Role、cache/Installation/Run identityを分離する。共通App互換を維持し、任意Secret ARN・cross-Tenant binding・自動fallbackを禁止する（[設計](docs/tenant-github-apps.md)、[ADR 0007](docs/adr/0007-tenant-github-apps.md)、Tasks 56–60。Task 56 metadata/API、Task 57 draft proof・active resolver/provider・generation再認可、Task 58 opaque HMAC ingress/integration-scoped inbox、Task 59 Installation binding/Run identity pin schemaを実装。OAuth state/provider、AWS-backed webhook resolver・exact-purpose IAM、旧Run/Installation backfill・drain、E2E/運用検証は未完了。owned modeはdefault-off）。

- [ ] GitHub Appを登録し、Secrets Manager保管、最小権限、Installation tokenの短期利用・失効方針を実装する（OAuth client secret取得に加え、App private keyのAWSCURRENT取得、RS256 JWT、Tenant/Repository認可、Repository一件・contents read限定Installation tokenの短命メモリcacheを実装。実App登録・専用role配備・worker接続は未完了。[資格情報と運用手順](docs/github-app-credentials.md)）。
- [ ] TenantとGitHub App Installation/Repositoryを対応付け、Repository Registry APIとRBACを追加する（Owner/Admin向けclaim/list、Repository Registry、active binding限定同期、scheduled processorに加え、Setup URL→one-time state→OAuth user token→user-visible Installation照合→active bindingのcallbackを実装。Terraformは明示opt-in。詳細は[GitHub Installation Setup](docs/github-installation-setup.md)）。
- [ ] Webhook ingressでraw bodyのHMAC-SHA256検証、Delivery ID冪等化、サイズ制限、監査、再送を実装する（署名先行検証・allowlist・durable inbox・Tenant Owner/Adminおよびplatform `hako-admin`のredacted listing/audited replayを実装し、retention/replay/API境界を含むPostgreSQL E2Eも追加。production operator provisioningは未実施）。[Hako Software Factory](docs/software-factory.md)。
- [ ] GitHub webhookをversion付きHako Repository Eventへ正規化し、durable inbox/outbox経由で処理する（Installation lifecycle/repository同期、7日raw payload purge、current-binding再検証、default-off per-repository push/PR verification Run生成とDelivery ID冪等Run/Check/Outboxを実装。focused E2Eは`make test-webhook-lifecycle-integration`。`workflow_job`実行はTask 52、実AWS配備とLambda MicroVM worker接続は未完了）。詳細は[Webhook Processor](docs/github-webhook-processor.md)。
- [ ] Job/Agent/Preview Runの共通状態・Operation・timeout/cancel/log/artifact metadataを設計する（WorkloadRun domain/DSQL/store、Tenant+repository binding、revision付きstate transition、timeout/cancel/artifact、Tenant requester visibility付きAPI、監査付きcreate/dispatch、consumer core、固定commit checkout、durable result/journal/cancel/recoveryを実装。Task 59では新規RunにTenant App integration/revision/generation pinを永続化し、owned-to-shared fallbackを拒否。EC2は対象外、基盤はLambda MicroVM。Task 61 image/container/network PoC、Task 45 agent、Task 40 worker/recovery配備は未完了。Run作成APIはopt-in default-off）。[Workload Run設計](docs/software-factory-workloads.md)。
- [x] Hako CLIにWorkload Runの作成・一覧・詳細・キャンセル操作を実装する（create API/CLIはopt-in default-off。実行consumer/runtimeは未実装）。[Workload Run設計](docs/software-factory-workloads.md)。
- [x] Workload artifactをTenant/Run/artifact/Resource Planeに束縛した短期ticketでGatewayから直接取得する。CLIはsize/SHA-256を検証してmode-0600でatomic保存し、Gateway IAMは設定済みS3 prefixのみに限定する。Agent変更提案は成功Runの一意な`agent_changes` artifactを選択し、TicketとRun metadataの一致を確認してから取得する。提案はuntrustedとして保存し自動適用しない。AWS配備と実S3取得は未検証（[Task 87](tasks/task-87-workload-artifact-download.md)）。
- [ ] Fake Runtime上でRepository commitに対する`go test ./...` Jobを実行し、結果とredacted logsを保存する（commit照合・隔離Podman実行・redaction・DSQL状態/metadata保存と、SSE-S3/SSE-KMS対応のS3 writerまで実装。bucket/IAM provisioning、worker接続、scheduler連携は未実装）。[Workload Run設計](docs/software-factory-workloads.md)。
- [ ] GitHub Checks APIでqueued/in-progress/completed check、commit SHA照合、summary/annotationを返す。
- [ ] GitHub Actions `workflow_job.queued`を受け、専用権限でephemeral runnerを登録し、1 Job実行後にderegister・Runtime cleanupする（Task 52でdurable admission/claim、Tenant broker token、runner status correlation、Plane-fenced cleanup worker、mTLS broker deregistration、AWS SDK Lambda MicroVM terminator、Resource Plane SQS Lambda handler/runtime composition、ARM64 ZIP build target、Outbox Dispatcherからのdefault-off startup queue publisher、Terraformのdefault-off queue/DLQ/exact IAM/lambda event-source mapping、integration/job/attempt単位のdurable Checkとshared Checks consumer連携を実装。PostgreSQL webhook→Check outbox→Dispatcher→fake GitHubのdelivery/ID persistenceをローカル検証。AWS apply、multi-Plane placement、guest listener/image、full runner cleanup E2E、live AWS/GitHub検証は未完了。[queue and Check contract](docs/actions-runner-startup-queue.md)）。
- [ ] `.hako/factory.yaml`のversioned strict schema、allowlisted step、network/secret policyを実装し、既存GitHub Actionsと共存させる（Task 53でstrict version 1 parser、push/PR selector、固定Catalog command plan、per-pipeline terminal result/Checksを実装。Issue commandはTenant member/Repository pin/immutable SHAを検証し、Agent Run/taskを作成・Workerへ安全に渡すところまで実装。Resource Plane runtime profileはWorkload kind単位に分離し、Job profileのAgent流用を拒否。Tenant別`native_agent`資格情報の目的分離API/CLI、root `hako-agent`からUID/GID 65532へdropするprocess境界、default-offのJob rootless containerd supervisor/ARM64 image build targetを追加。Broker解決・実行時credential配布は無効。固定ツール限定のNative Agent factoryを`HAKO_NATIVE_AGENT_ENABLED=true`と固定モデル指定時だけGuestへ接続し、既定ではAgent credentialを受け付けない。成功Agent Runのattempt-bound `agent_changes` ZIP/S3 metadata/download経路はlocal実装済み。安全なbranch/PR提出、サービス宣言のruntime接続、MicroVM network/resource enforcementは未完了。実行とScheduler有効化は引き続き無効）。
- [ ] IssueからAgent Runを実行し、明示的な最小write permissionでbranch/PRを作成するworkflowを追加する（Repository branch/PR policy API/CLIと認可判定は実装済みだが`execution_enabled=false`。Agent scheduling、model execution/credential policy、write token発行、runtime enforcement、human review flowは未完了）。
- [ ] PR Preview Workspace、artifact/SBOM storage、OIDC deploy policyを分離タスクとして実装する（Tenant Software Catalog API/CLI、legacy/immutable-ID GitHub OIDC RS256/JWKS claim検証API、Tenant別Preview opt-in/PR desired-state lifecycle、quota-based expiry/default-off sweep、Tenant member向けbounded Preview list API/CLI、retry/backoffとfenced claim lease、cleanup confirmationまでquotaを保持するworker orchestration、Environment reviewer保護とOIDC claimにpinしたlive Actions run/承認履歴検証、API→mTLS Credential Broker配線、PR head世代ごとのprivate Runtime allocation metadata、世代別cleanup fence/runtime Workspace ID/claim+lease、Tenant member Preview Gateway Session API/CLI、期限付きTicketとclose/head update時のactive-session失効、Preview Sessionを使ったloopback-only `hako github repository previews open`、Preview専用`preview-logs` Sessionと最大256 KiBのread-only Guest log tail、Tenant placement policy/health/capacityを使ったPreview Plane選定とcleanup確認後のcapacity解放、Lambda MicroVM Resource Controllerのbroker checkout→immutable source archive→S3→Guest one-use WSS install compositionを追加。Resource Plane queue/source deploymentとexact IAMはdefault-off Terraformで定義し、Preview/Gatewayのimage・journal・origin・ticket key一致を検証。残りはmatching ARM64 Guest imageと実MicroVM/GatewayのAWS検証、production schedule/queuesの配備、AWS STS trust/exchange。検証APIは常に`execution_enabled=false`）。[Preview lifecycle](docs/preview-lifecycle.md)、[Task 88](tasks/task-88-preview-application-logs.md)
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
