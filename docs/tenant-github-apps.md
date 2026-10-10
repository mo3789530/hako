# Tenant別GitHub App設計

Status: **Tasks 56–59を部分実装。Tenant Appの有効化/運用は未完了でdefault-off**。Draft proof API/credential libraries、Webhook resolver/authorization/generation再検証、Installation/Run identityに加え、OAuth state pinとcallback adapterを追加。API binaryはprivate-key reader roleを直接assumeせず、trusted credential brokerがない場合はverifyを無効扱いにする。Tenant Webhook routeは専用flagとWebhook-purpose reader Role ARN allowlistでopt-in可能だが、real role trust/Secret provisioningは未実施。OAuth-purpose brokerのproduction接続、CLI setup、legacy backfill/drain、E2Eは残る。現行共通App方式は維持する。
決定事項は[ADR 0007](adr/0007-tenant-github-apps.md)、既存資格情報実装は
[GitHub App credentials](github-app-credentials.md)を参照。

## 1. 範囲と不変条件

- 共通Hako Appの`shared`と、別App・別Secretを持つ`tenant_owned`を選択可能にする。
- 初期版はGitHub.com、Hako管理の承認済みAWS account/region、Tenantごとに一active default integration。Customer accountのSecret/BYOC、複数active App、一般Workspace Secretは対象外。
- private key、OAuth client secret、Webhook HMAC secretは独立したSecretStringとする。DSQLにはmetadata/ref/versionだけを保存する。API、Outbox、SQS、Run、CLI設定、ログ、ArtifactにSecret値は入れない。
- App IDやInstallation IDを知っているだけでは設定・token取得を許可しない。別Tenantに対する権限がない利用者には既存APIと同じhidden 404を返す。
- 一つのGitHub App IDは一app registrationへ一意に登録する。tenant_owned registrationのowner Tenantは一つ。共通App IDをtenant_ownedとして再登録することも拒否する。
- Installationの既存global uniquenessは維持する。App IDを足して同じInstallationを別Tenantへ再bindingできるようにはしない。

## 2. データモデル（論理schema、DDLはTask 56/59）

```text
Tenant
  └─ GitHub Integration ── App Registration
         │                     └─ Approved Credential Binding
         ├─ Installation Binding
         ├─ Repository
         └─ Workload Run（integrationを固定）
```

| モデル | 主なmetadataと制約 |
|---|---|
| `github_app_registrations` | id、github_app_id（unique）、mode、owner_tenant_id（owned必須/sharedはNULL）、slug、OAuth client_id、status、revision。API base URLは固定GitHub.com |
| `tenant_github_integrations` | id、tenant_id、app_registration_id、status（draft/verifying/ready/active/disabled）、config_revision、credential_generation、opaque webhook_route_id、created_by/timestamps。`(tenant_id,id)` unique |
| Tenant default selection | tenant_id一行、integration_id、revision。OCC transactionで一つのactive選択を保護し、同Tenant・ready integrationのみ選択可能 |
| `github_credential_bindings` | 運用側承認済みid、tenant/app owner、用途別Secret ARNとapproved version、用途別reader_role_arn、必要なKMS ref、status。共有Appではplatform所有を明示 |
| Installation/Repository | Tenant-owned modeは`tenant_github_app_installation_bindings`でintegrationと既存Tenant Installationを結ぶ。Repositoryは従来どおりTenant/Installationに属する |
| OAuth setup state | 既存Tenant/user/hash/PKCE/expiryにintegration_id、config_revision、credential_generationを追加。callbackで再検証し変更後のstateを拒否 |
| Webhook Inbox | integration_id、検証したcredential generation、delivery_id、digest。dedupは`(integration_id, delivery_id)`。同IDで異なるdigestは競合として隔離 |
| Workload Run/dispatch | サーバー解決したintegration_id、App registration、config_revision/credential_generationを固定。Run処理中に別Appへ差替えない。Secret ARNやcredentialはmessageに載せない |

registration ID、integration ID、credential binding IDは別物。表示App IDとSecret解決用IDを混同しない。
Installation binding、Workload Run pin、OAuth setup pinのDDLはTasks 58/59のmigrationで追加済み。Webhook processor再検証も実装済み。旧Installation/Runのbackfillと配備時のmigration runbookは未完了。

## 3. 設定APIとRBAC

| API | 権限・契約 |
|---|---|
| `GET /v1/tenants/{tenant}/github/apps` | Owner/Admin。mode、App ID、slug、integration status、revisionだけ返す |
| `POST /v1/tenants/{tenant}/github/apps` | Owner/Admin。App ID/slug/client_idと**承認済みcredential_binding_id**を指定しdraft作成。raw Secret/任意ARN/Role/URLは拒否 |
| `GET /v1/tenants/{tenant}/github/apps/{integration}` | Owner/Admin。sanitized metadataのみ。別Tenant IDはhidden 404 |
| `POST .../{integration}/verify` | Owner/Admin。revision指定。trusted brokerがApp JWTの`GET /app`でApp IDと承認bundleを検証。外部副作用はDB transaction外 |
| `POST .../{integration}/select-default` | Owner/Admin。Tenant selectionのexpected revision指定、ready状態のみ。既存binding/Runの切替は下記migration制約に従う |
| `POST .../{integration}/disable` | Owner/Admin。expected revision指定。新規setup/token/Runを拒否、state/cache無効化とcancel/reconcile要求をOutboxに保存 |
| `POST .../{integration}/setup` | Owner/Admin。対象integrationに固定した既存PKCE setup flowを開始 |

OAuth callbackは既存共通routeを維持し、one-time stateから対象integrationを解決する。callback queryのTenant/App IDでverifierを選ばない。
Webhookは`POST /v1/integrations/github/apps/{integration_id}/webhook`を別途追加する。

POSTにはidempotency key、更新にはexpected revisionを用い、競合は409。未承認binding、別Tenant/App所有bindingはSecret取得前に拒否する。
App verificationとdefault選択は別操作であり、GitHub疎通成功だけでRunを起動しない。
Task 56では`hako github app list/add/show/verify/select-default/disable`からmetadataを操作する。API routeは`HAKO_GITHUB_APP_MANAGEMENT_ENABLED=true`で明示的に有効化し、未設定時はrouteを登録しない。Task 57ではtrusted broker proof adapterを構成したときにのみ`verify`がdraftからreadyへ進む。標準API binaryはprivate-key accessを持たず、adapter未構成なら409 `github_app_not_ready`、証明失敗なら秘匿された502 `github_app_verification_failed`を返す。Tenant Webhook ingressは`HAKO_TENANT_GITHUB_WEBHOOK_ENABLED=true`とTerraformのexact Webhook-role allowlistで別途opt-inする。`select-default`は未実装のため409を返す。CLI/APIにSecret値を入力させない。

`add`は`Idempotency-Key`必須で、`disable`/`verify`/`select-default`は`expected_revision`を要求する。Owner/Admin以外と別Tenantのresourceはhidden 404。詳細なpath、body、response、error codeは[Control Plane API Contract](api-contract.md#tenant-github-app-metadata)を参照。

## 4. Secret provisioningとIAM境界

1. Tenant App管理者がGitHubで別Appを作成する。運用担当が認証された別経路で三種類のSecretを受領・保管し、App ID/Tenant/用途とreader Roleを承認する。Hako管理APIへのraw key uploadは提供しない。
2. 運用側provisionerがSecret/Roleを作成しcredential binding metadataを登録する。Task 56では`github_credential_bindings`に対するAPI roleの権限をSELECTだけに制限する。登録はDSQL admin/operatorの別認証で、Tenant・App IDと三つの用途別Secret referenceを確認した後に行う。Tenant Owner/Adminは自分のTenant/App IDに一致する`approved` binding IDだけを指定できる。ARN文字列のprefix一致だけでは承認扱いにしない。

operator登録のparameterized SQL templateは次のとおり。`:name`は運用ツールがbindする値を表し、psqlへそのまま渡す構文ではない。API経由で実行せず、provisioning/approval記録を確認した運用者が実行する。列にはSecret値ではなく、承認済みresource referenceだけを保存する。

```sql
INSERT INTO github_credential_bindings (
  id, tenant_id, github_app_id, status,
  private_key_secret_ref, oauth_secret_ref, webhook_secret_ref,
  private_key_reader_role_arn, private_key_secret_version_id, approved_at
) VALUES (
  :binding_id, :tenant_id, :github_app_id, 'approved',
  :private_key_secret_ref, :oauth_secret_ref, :webhook_secret_ref,
  :private_key_reader_role_arn, :private_key_secret_version_id, :approved_at
);
```

Task 57ではTenant-owned private key用に`private_key_reader_role_arn`と`private_key_secret_version_id`を追加した。resolverは`role/`のexact IAM Role ARN（wildcard不可）、完全なSecrets Manager Secret ARN、32桁hexのimmutable Version IDを要求し、`AWSCURRENT`や欠落pinを拒否する。`TenantPrivateKeySecretAPI`はSTS AssumeRoleにSecret ARN限定のsession policyを付け、Assumed Roleの短期credentialで固定VersionIdだけを取得し、AWS/GitHubエラーを外へ漏らさない。Tenant App token clientのcache identityにはTenant/integration/App/revision/generationを含め、active revision/generationとTenant/App ownerをDBで都度再検証するため、古いgenerationのclientはcached tokenも返せない。共有Appや既存credential clientはこのmetadataを使わず、従来のAWSCURRENT方式を維持する。AWSアクセスはproviderを実際のtrusted serviceへ接続した時のみ発生し、テストはfake providerを使う。

Bindingの更新・停止とIAM/Secret provisioningの自動化はこのTaskの範囲外。APIにはbinding ID以外のARN/Role/URLを渡せず、承認bindingが見つからない場合はSecret accessより前に拒否する。
3. CP brokerはauthoritative Tenant/integration/bindingからRoleを解決する。取得Roleはprivate-key、OAuth交換、Webhook検証の用途別に分け、それぞれexact ARNの`GetSecretValue`と必要時の限定KMS decryptのみを許可する。
4. API/Job VM/RPにTenant Secretの包括的読取Roleを渡さない。broker Roleだけが承認reader Roleに対する限定`sts:AssumeRole`を持つ。Tenant/用途/session識別を監査し、利用者からRole ARN/session tagを受け取らない。
5. Credential readerにはSecretの作成/更新/tag変更、IAM変更、compute操作権限を与えない。Secret承認・rotationとreadは別権限。

Secret値の受領・GitHub設定変更・AWS provisioningは明示承認を要する運用作業。
ローカルテストでは全てfake Secret providerで代替する。Terraform implementation/validate/planはTask 59、applyは別承認。
broker侵害のblast radiusは承認済み全Roleに及び得る。Tenant別Secret/IAMは共有brokerプロセスの完全隔離を保証しない。

## 5. Token resolverとcache

- `TenantAppResolver`相当がRunからTenant/integrationを読み、active状態、App ownership、Installation/Repositoryの同一integration、revision/generationを確認する。callerが渡したApp IDやdefaultだけで選ばない。
- Task 37のclientを、検証済みbindingから得たApp ID/Secret providerで生成する。Jobにtokenを配布せず、trusted checkoutのfetch境界だけで使う。
- client pool/cache keyに`tenant_id + integration_id + github_app_id + config_revision + credential_generation + installation_id + repository_id`を含める。Role/provider、OAuth verifier、Webhook secret cacheもrevision/generationを含めて分離し、上限/expiryを設ける。
- 現行のcache再利用前DB/live GitHub検証、read-only repository scope、2分前refresh、safe errors、bounded retryを維持する。GitHub rate-limit cooldownはApp単位でも集約する。
- Secret読取失敗、disabled、revision不一致、App ID不一致、cross-Tenant bindingはfail closed。**legacy環境変数・共通Appへのfallbackは禁止**。
- `ClearCache`だけでは複数replicaへ伝播しない。DB generationを毎回検証してstale clientの返却を止め、invalidationイベントは補助とする。返却直後の失効は避けられないためfetch側も失効時に停止する。

## 6. OAuthとWebhook

OAuth client_id/client-secret/slugはstateで固定したintegrationから解決する。callbackの一時user tokenでuser-visible Installationを確認し、expected App ID・installation・accountに照合する。
完了transactionでOwner/Admin資格、Tenant ownership、active integration、revision/generation、one-time stateを再検証する。変更前に開始したcallbackはやり直す。

migration `000062`でstate pinと再検証を実装。APIにtrusted `TenantGitHubSetupResolver`を明示注入した場合のみowned setup routeを登録し、callbackでstored pinからverifierを選ぶ。App不一致、失効、shared fallback、owned bindingの再assignmentを拒否する。標準API binaryにそのresolverはまだ接続しておらず、production OAuth-purpose brokerとCLI setupは残る。詳細は[Installation setup](github-installation-setup.md)。

Webhook routingは次の順序を固定する。

1. opaque routeを候補integrationへ解決する。routeは秘密鍵でも認可証明でもない。unknown/disabled routeに安全な拒否・rate limitを適用する。
2. bounded raw bodyを改変せずHMAC-SHA256で検証する。未検証payloadを根拠にSecret/Role/Tenantを選ばない。全TenantのSecretを順番に試すこともしない。
3. 検証後にschema/eventをdecodeし、payloadのApp ID（存在する場合）とInstallationがroute側registrationに一致するか確認する。active binding前のinstallation eventはそのintegrationのinboxへ保存するだけでTenant bindingを自動生成しない。
4. `(integration_id, delivery_id)`でdurable inboxへ保存し、processorが同Tenant/integrationのstateだけを更新する。別Appの署名や同じDelivery IDで異なるpayloadは拒否/隔離する。

既存共通Webhook URLはshared専用aliasとして保持する。Tenant-ownedの処理失敗を共通secretで再検証しない。
公開ingressにApp private key/OAuth client secretを読ませず、Webhook-purpose readerまたは内部検証brokerを使う。

実装状況: Tenant ingress handlerはopaque route解決、raw-body HMAC、current/previous secretの期限付き検証、HMAC後のgeneration/Installation authorizer、integration/generation付きdurable inbox handoffを実装済み。DB resolverは承認bindingのWebhook-purpose reader Role、Secret ARN、固定VersionIdだけを解決し、STS readerはそのSecretに限定したsession policyでcurrent/previousの値を取得する。Installation payloadにApp IDが含まれる場合は正の整数として保存し、受信時とProcessorでroute側App IDとの一致を検証する。ProcessorはgenerationとInstallation bindingも再検証し、失効・不一致イベントをfailedへ隔離する。DB照会エラーは再試行対象として返し、失効として隔離しない。API route/Terraformはdefault-offで、実IAM trust/Secret provisioningとproduction OAuth broker接続は未実施。

## 7. ローテーション・停止

- Secret versionをprepare→GitHub疎通/署名検証→approved binding/generationをOCC更新→cache失効の順で切替する。DB更新とSecret version操作はatomicではないため、途中失敗時は新規利用を止めてreconcileする。
- 初期版はTenant-owned Secretを承認済みversion IDに固定する。無承認のAWSCURRENT差替えを自動採用しない。Task 37のshared clientはAWSCURRENT方式を維持するが、owned用provider/version pinningをTask 57で追加する。
- Webhook secretはcurrent/previousの最大二versionを、そのintegrationでのみ重ねて受理可能。previous猶予は初期値10分・運用上限24時間、期限後は拒否する。GitHub側変更も別運用操作でありHako DBだけで完了しない。
- private-key変更は新JWT/トークン発行に反映するが、既存Installation tokenは自動失効しない。既知token revoke、App suspend/uninstall、全replica停止は別操作としてrunbookに記載する。
- disableは新規発行を停止し、in-flight Runにはcancel/cleanupを依頼する。既発行tokenの有効性と実VM停止をDB statusだけで保証しない。
- key/secret referenceの変更と、App ID変更は区別する。**App IDはregistration作成後immutable**。別Appへの移行は新integrationと再installationを要する。

## 8. 既存データ移行と切替

1. additive schemaを導入し、既存共通App metadataを一shared registrationとして登録する。環境App ID未設定なら既存データを推測して有効化せず運用確認を要求する。
2. 既存Tenantごとのshared integrationを作り、Installation/Repository/Inbox/Runを正しいshared integrationへbackfillする。既存global Installation uniquenessは維持する。特定integrationを判定不能な履歴はquarantineし、安易なdefault紐付けをしない。
3. 移行前に開始したOAuth stateは失効させ再setupさせる。queued/running Runはshared identityを固定してdrainするかcancel/cleanupを確認する。Appを差し替えて再利用しない。
4. 両modeを扱うread/write codeとbackfill検証を配備してからNOT NULL/複合制約を追加する。古いworkerが新owned Runを誤処理しないようversion付きcommandとcapability gateを設ける。
5. owned Appを検証・再installation・repository同期した後、旧integrationの非終端Runをdrain/cancelしdefaultをOCC transactionで切り替える。旧bindingを新Appへ書換えない。初期版は既存Repository registryとの衝突を解消するまでownedのactivationを禁止する。
6. ロールバックはsharedへの**明示的**選択とdrain手順で行う。owned Secret障害時の自動fallback、履歴削除、App IDの書換えはしない。

## 9. 検証とリリース条件

Task 56–60で以下を順に検証する。全ケースでSecret値をassertionの出力に含めない。

- API/RBAC: Owner/Adminのみ、member/別Tenant hidden 404、idempotency/OCC、他Tenant/Appの承認binding・任意ARNの拒否。
- credential: Tenant A/Bで別App/key/token、誤Secret/Role/Version、app proof mismatch、cache/revision切替、旧clientの利用禁止、no-fallback。
- setup/webhook: Tenant/Appに固定したstate、cross-App callback、偽署名・誤route・replay、旧generation、suspend/delete、rotation猶予、同Delivery IDの別integration独立処理。
- migration: 現行schema fixtureのbackfill、global Installation uniqueness、既存共通経路、queued Run固定、途中失敗/drain/rollback、判定不能履歴。
- fake Secret provider + fake GitHub HTTP + 隔離Postgres + API/CLI E2Eを必須にする。IAM policy/plan検証はexact ARNとrole分離を確認する。実AWS/GitHub smokeは別承認で行う。

Tenant-owned機能はTask 60完了までdefault-off。本設計だけでTask 37の現行資格情報実装や既存Tenantを変更したことにはしない。
