# Workspace上限と閲覧権限

この文書はMVPのWorkspace quotaと閲覧認可を記録します。Tenantごと最大10件のQuotaはWorkspace storeで強制され、`POST /v1/tenants/{tenant_id}/workspaces`は超過時に`workspace_quota_exceeded` / HTTP 409を返します。Workspace一覧・取得APIも以下のRBACを強制します。

## Workspace quota

- 上限は**Tenantあたり10件**とする。
- `observed_state != deleted`のWorkspaceを数える。running/suspended/provisioning/failedを含み、削除要求中も実リソースの削除完了が確認されるまでは枠を消費する。
- 11件目の作成を拒否する。Storeは`workspaces.ErrWorkspaceQuotaExceeded`を返し、作成APIは安定した`workspace_quota_exceeded`エラー（HTTP 409）へ変換する。
- 同時に複数の作成要求が来ても10件を超えないこと。Tenantごと10個の一意なquota slotをWorkspace/Operation/Outboxと同じtransactionで確保する。Aurora DSQLで使えない`SELECT FOR UPDATE`には依存しない。
- quota適用の単位はTenantであり、Owner個人単位ではない。

QuotaはTenantごとの最大10個のslot rowで表し、一意制約を使って作成競合を調停します。`SELECT FOR UPDATE`には依存しません。Quota migration適用前から存在するWorkspaceは、新規作成transaction内でactive件数を確認してslotへ遅延backfillします。`observed_state=deleted`の既存行に残ったslotも作成時に掃除します。

## 閲覧権限

確定したWorkspace閲覧ルール:

- Tenant `member`は自身が`owner_id`であるWorkspaceのみ一覧・取得できる。
- Tenant `owner`と`admin`は同じTenant内のWorkspaceを一覧・取得できる。
- どのRoleでも別TenantのWorkspaceにはアクセスできない。
- Workspace作成、一覧、取得、Session発行などすべての入口で同じ認可規則を適用する。

Owner/AdminのTenant内全Workspace閲覧とMemberのown-only閲覧を採用します。権限はCognito claimではなく、各一覧・取得要求時の`tenant_members` roleから判定します。Tenant membershipと可視Workspace取得は同一DB transactionで確認します。取得APIでは、存在しないWorkspaceとMemberから見えないWorkspaceを同じ404で返します。

## Workspace APIとページネーション

- `GET /v1/tenants/{tenant_id}/workspaces`は一覧を返す。既定`limit=20`、許容範囲は1〜100、`offset`は0〜1,000,000。
- `GET /v1/tenants/{tenant_id}/workspaces/{workspace_id}`は単一Workspaceを返す。
- 一覧は`created_at DESC, id DESC`で安定ソートし、`items`, `total`, `limit`, `offset`を返す。
- CLIは`hako list <tenant-id> [limit] [offset]`と`hako get <tenant-id> <workspace-id>`を使う。

## 受け入れ条件

- Tenant内で10件までは作成でき、11件目は安定したquotaエラーになる。
- 並行作成テストでもTenantごとに上限を超えない。
- Workspaceが削除完了（`observed_state=deleted`）になると同じtransactionでquota slotを解放する。
- Memberは自分のWorkspaceだけ、Owner/AdminはTenant内すべてを閲覧でき、別Tenantにはアクセスできない。integration testでrole別一覧・取得と非所有Workspaceの拒否を検証する。
- quotaと閲覧ルールをCLI/APIの利用者向けドキュメントに反映する。

Store quotaのintegration testは並行16要求に対して10件だけを許可し、削除完了後に枠が再利用可能なことを確認します。API integration testは作成の冪等再送と11件目の安定したHTTP 409に加え、Member/Owner/Adminごとの一覧・取得、非所有・別Tenant Workspaceの404、ページ上限拒否を確認します。
