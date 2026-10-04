# ADR 0007: Tenant専用GitHub Appと資格情報の分離

- Status: Accepted (design only; Tasks 56–60未実装)
- Date: 2026-10-04
- Scope: GitHub.comのTenant-owned App。一般Workspace Secret管理、GitHub Enterprise、Customer AWS AccountのSecret取得は別設計。

## Context

現行は共通のHako AppをTenantごとのInstallationへ紐付ける。Task 37のTokenClientは一インスタンスに一App ID・一private-key ARNであり、token cacheのみTenant/Installation/Repository別である。OAuth verifierとWebhook ingressも共通設定である。

秘密鍵を別Secretにコピーするだけでは同じGitHub Appの権限・侵害範囲は分離されない。Tenant別の資格情報境界が必要な場合はApp自体を分ける。ただし既存共通AppのInstallationを無断で交換してはならない。

## Decision

`shared`と`tenant_owned`を明示的に併存させる。既存Tenantはsharedを維持し、新規Tenant-owned選択はdefault-offで段階導入する。同一Tenantには初期版で一つのactive default integrationを許可する。切替準備用draftと過去の無効integrationは保持できる。

| 対象 | shared | tenant_owned |
|---|---|---|
| GitHub App | Hako共通App | Tenant専用App。別Tenantとの共有登録は拒否 |
| private key / OAuth secret / Webhook secret | 共通Appの承認済みSecret bundle | Tenant専用の三Secretと承認済み読取Role |
| Installation binding | 各Installationは一Tenantのみ | 同じ制約に加えApp/integration一致が必須 |
| Runとtoken cache | Tenant分離を維持 | Tenant + integration + App + revision/generation分離 |

詳細なAPI、schema、routing、移行、受入条件は[設計仕様](../tenant-github-apps.md)を正本とする。

### 取得権限とrouting

- Tenant Owner/Adminだけがintegration設定・検証・disable・切替を要求できる。ユーザーAWS鍵、raw PEM、OAuth secret、Webhook secretをHakoの管理APIへ送らない。
- Secret/Role参照は運用側がTenant・App・用途を検証して登録したcredential binding IDから解決する。利用者任意ARNやApp IDだけを根拠にcredentialを取得しない。
- CPのtrusted credential brokerは必要なTenant用RoleだけをAssumeRoleして取得する。これは資格情報取得のためであり、CPからResource Plane computeを操作する権限を付与するものではない。Job VMとRPにはApp key/DSQLへのアクセスを与えない。
- Tenant-owned Roleは用途別のexact Secret ARN、必要時だけexact KMS keyを許可する。API roleにはTenant鍵の包括的読取権限を追加しない。Roleへの信頼設定、binding更新、Secret tag変更は運用側のみ。
- Webhookはopaque integration routeで候補Secretを選び、raw bodyのHMAC検証後にのみTenantへ関連付ける。payloadのTenant/App/Installation IDやURLのIDは認可の根拠ではない。[GitHub webhook validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
- 専用設定が不正・停止・失効したらfail closed。共通Appや別TenantのSecretへfallbackしない。

## Consequences and alternatives

共通Appのみよりsetup、Webhook、IAM、ローテーション、migrationが複雑になる。運用側のSecret provisioningと承認が必要で、初期版はセルフサービスのraw Secret uploadやcross-account Secretを提供しない。

別Secretだけに同じApp鍵を複製する案はApp権限分離にならず不採用。全Secretを読める共有API role + コード上のTenant filterも侵害範囲が大きいため不採用。ABACは将来の運用最適化候補だが、tag変更権限による越境を検証するまではexact ARN方式を採用する。[AWS Secrets Manager ABAC](https://docs.aws.amazon.com/secretsmanager/latest/userguide/auth-and-access-abac.html)

broker自身は複数TenantのRoleを取得できる高信頼境界であり、完全なプロセス隔離にはならない。broker侵害も分離したいEnterprise要件にはTenant別service/account deploymentが必要で、初期版の保証外とする。

本ADRは設計承認のみ。実App作成、秘密情報取得、Role/Secret provisioning、AWS apply、既存Tenantのcutoverは行わない。
